package bashsandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/procexec"
	"github.com/pilat/coagent/internal/shellenv"
)

const (
	bashExecutable       = "bash"
	devPath              = "/dev"
	preflightOutputLimit = 8 * 1024
	preflightTimeout     = 3 * time.Second
)

// enforcement caches the backend confinement probe: it spawns sandboxed
// processes and its outcome cannot change while the process runs.
var enforcement struct {
	once sync.Once
	err  error
}

// Config configures session process filesystem write confinement.
type Config struct {
	Enabled                     bool
	ProjectID                   int64
	WorkDir                     string
	CanonicalWorkDir            string
	SessionKey                  string
	ReadScope                   ReadScope
	ExcludeSessionWritableRoots bool
	WritablePaths               []string
}

// Runner constructs processes with the configured sandbox policy.
type Runner interface {
	procexec.Runner

	// BashCommand builds a plain `bash -c` command that never sources a shell-env
	// snapshot. Internal helpers only (file mutation, probes) — use ShellCommand.
	BashCommand(ctx context.Context, command, workDir string, args ...string) (*exec.Cmd, error)

	// ShellCommand builds a user shell command, sourcing workDir's shell-env
	// snapshot when one is available so the command sees the project's activated
	// toolchain (mise/asdf/etc.). Falls back to plain `bash -c` otherwise.
	ShellCommand(ctx context.Context, command, workDir string) (*exec.Cmd, error)

	// WritableRoots reports the normalized filesystem roots the sandbox allows
	// writes under; nil when confinement is disabled.
	WritableRoots() []string
	ReadScope() ReadScope
}

var _ Runner = disabledRunner{}

// providerAware lets New attach the shell-env provider to a platform runner
// built by the provider-less runnerFactory, so probe runners never snapshot.
type providerAware interface {
	setProvider(p shellenv.Provider)
}

type runnerFactory func(policy processPolicy) (Runner, error)

type limitedBuffer struct {
	buffer bytes.Buffer
}

type shieldProbeFixture struct {
	base    string
	project string
	denied  string
	secret  string
}

type environmentEntry struct {
	name  string
	value string
	raw   string
}

type disabledRunner struct {
	provider  shellenv.Provider
	policyKey string
}

// New constructs a Bash command runner. provider may be nil: ShellCommand then
// degrades to plain `bash -c`.
func New(cfg Config, provider shellenv.Provider) (Runner, error) {
	if !cfg.Enabled {
		return disabledRunner{
			provider:  provider,
			policyKey: policyKey(nil, fmt.Sprintf("%d:%s", cfg.ReadScope, cfg.SessionKey)),
		}, nil
	}

	policy, err := preparePolicy(cfg)
	if err != nil {
		return nil, err
	}

	if err := Probe(); err != nil {
		return nil, err
	}

	runner, err := newEnabledRunner(policy)
	if err != nil {
		return nil, fmt.Errorf("create Bash sandbox runner: %w", err)
	}

	if aware, ok := runner.(providerAware); ok {
		aware.setProvider(provider)
	}

	return runner, nil
}

//nolint:wsl_v5 // Writable-root normalization is intentionally one fail-closed pipeline.
func preparePolicy(cfg Config) (processPolicy, error) {
	paths := make([]string, 0, len(cfg.WritablePaths)+4)
	paths = append(paths, cfg.WorkDir)
	if cfg.ReadScope == HostReadable && !cfg.ExcludeSessionWritableRoots {
		paths = append(paths, os.TempDir(), "/tmp")

		processesDir, err := processProjectWritableRoot(cfg.ProjectID)
		if err != nil {
			return processPolicy{}, err
		}
		if processesDir != "" {
			paths = append(paths, processesDir)
		}

		cacheDir, err := existingUserCacheDir()
		if err != nil {
			return processPolicy{}, fmt.Errorf("resolve user cache directory: %w", err)
		}

		if cacheDir != "" {
			paths = append(paths, cacheDir)
		}
	}

	if cfg.ReadScope == HostReadable {
		paths = append(paths, cfg.WritablePaths...)
	}

	writableRoots, err := normalizeWritableRoots(paths)
	if err != nil {
		return processPolicy{}, fmt.Errorf("normalize Bash sandbox writable roots: %w", err)
	}

	policy, err := buildProcessPolicy(cfg, writableRoots)
	if err != nil {
		return processPolicy{}, fmt.Errorf("build Bash sandbox policy: %w", err)
	}

	return policy, nil
}

func processProjectWritableRoot(projectID int64) (string, error) {
	if projectID <= 0 {
		return "", nil
	}

	processesDir, err := coagenthome.ProcessProjectDir(projectID)
	if err != nil {
		return "", fmt.Errorf("resolve process output directory: %w", err)
	}

	if err := os.MkdirAll(processesDir, 0o700); err != nil {
		return "", fmt.Errorf("create process output directory: %w", err)
	}

	return processesDir, nil
}

// Probe verifies that the platform backend actually confines writes: a write
// inside a writable root must succeed, one outside must fail and leave no file.
// It runs at most once per process — New calls it lazily, main calls it at
// startup so a backend that cannot enforce fails the daemon rather than the
// first session.
func Probe() error {
	enforcement.once.Do(func() {
		enforcement.err = probeEnforcement(newEnabledRunner)
		if enforcement.err == nil {
			enforcement.err = probeShieldEnforcement(newEnabledRunner)
		}
	})

	return enforcement.err
}

func (r disabledRunner) Command(ctx context.Context, request procexec.Request) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, request.Path, request.Args...)
	cmd.Dir = request.WorkDir
	cmd.Env = request.Env

	return cmd, nil
}

func (r disabledRunner) BashCommand(ctx context.Context, command, workDir string, args ...string) (*exec.Cmd, error) {
	return r.Command(ctx, procexec.Request{
		Path:    bashExecutable,
		Args:    append([]string{"-c", command}, args...),
		WorkDir: workDir,
	})
}

func (r disabledRunner) ShellCommand(ctx context.Context, command, workDir string) (*exec.Cmd, error) {
	shell, snap := snapshotFor(ctx, r.provider, workDir)
	if snap == "" {
		return r.BashCommand(ctx, command, workDir)
	}

	return r.Command(ctx, procexec.Request{
		Path:    shell,
		Args:    []string{"-c", sourceLine(snap, command)},
		WorkDir: workDir,
	})
}

func (disabledRunner) WritableRoots() []string { return nil }
func (r disabledRunner) PolicyKey() string     { return r.policyKey }
func (disabledRunner) ReadScope() ReadScope    { return HostReadable }

// snapshotFor resolves the shell and snapshot path for workDir, or ("", "") when
// snapshotting is unavailable (nil provider or graceful degradation).
func snapshotFor(ctx context.Context, provider shellenv.Provider, workDir string) (string, string) {
	if provider == nil {
		return "", ""
	}

	snap := provider.Snapshot(ctx, workDir)
	if snap == "" {
		return "", ""
	}

	return provider.Shell(), snap
}

// sourceLine builds `source <snap>; <command>` with the snapshot path quoted.
func sourceLine(snap, command string) string {
	return "source " + shellPath(snap) + "; " + command
}

//nolint:wsl_v5 // Environment validation and PWD replacement are one normalization pass.
func innerEnvironment(configured []string, workDir string) ([]environmentEntry, error) {
	values := configured
	if values == nil {
		values = os.Environ()
	}

	entries := make([]environmentEntry, 0, len(values)+1)
	foundPWD := false
	for _, raw := range values {
		name, value, ok := strings.Cut(raw, "=")
		if !ok || name == "" || strings.ContainsRune(name, '\x00') || strings.ContainsRune(value, '\x00') {
			return nil, fmt.Errorf("invalid process environment entry %q", raw)
		}
		if name == "PWD" {
			value = workDir
			raw = name + "=" + value
			foundPWD = true
		}
		entries = append(entries, environmentEntry{name: name, value: value, raw: raw})
	}
	if !foundPWD {
		entries = append(entries, environmentEntry{name: "PWD", value: workDir, raw: "PWD=" + workDir})
	}

	return entries, nil
}

func sandboxLauncherEnvironment() []string {
	return []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	originalLen := len(data)

	remaining := preflightOutputLimit - b.buffer.Len()
	if remaining > 0 {
		if len(data) > remaining {
			data = data[:remaining]
		}

		_, _ = b.buffer.Write(data)
	}

	return originalLen, nil
}

func (b *limitedBuffer) String() string {
	return b.buffer.String()
}

func normalizeWritableRoots(paths []string) ([]string, error) {
	roots := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))

	for _, path := range paths {
		root, err := normalizeWritableRoot(path)
		if err != nil {
			return nil, err
		}

		if _, ok := seen[root]; ok {
			continue
		}

		seen[root] = struct{}{}
		roots = append(roots, root)
	}

	sort.Slice(roots, func(i, j int) bool {
		depthI := strings.Count(roots[i], string(os.PathSeparator))

		depthJ := strings.Count(roots[j], string(os.PathSeparator))
		if depthI != depthJ {
			return depthI < depthJ
		}

		return roots[i] < roots[j]
	})

	return roots, nil
}

func normalizeWritableRoot(path string) (string, error) {
	expanded, err := expandHome(path)
	if err != nil {
		return "", err
	}

	if !filepath.IsAbs(expanded) {
		return "", fmt.Errorf("writable path %q must be absolute", path)
	}

	resolved, err := filepath.EvalSymlinks(filepath.Clean(expanded))
	if err != nil {
		return "", fmt.Errorf("resolve writable path %q: %w", path, err)
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat writable path %q: %w", path, err)
	}

	if !info.IsDir() {
		return "", fmt.Errorf("writable path %q is not a directory", path)
	}

	if filepath.Dir(resolved) == resolved {
		return "", fmt.Errorf("writable path %q resolves to filesystem root", path)
	}

	if runtime.GOOS == "linux" {
		for _, protectedRoot := range []string{"/proc", devPath, "/sys"} {
			if pathWithinRoot(resolved, protectedRoot) {
				return "", fmt.Errorf("writable path %q cannot be under protected Linux root %q", path, protectedRoot)
			}
		}
	}

	return resolved, nil
}

func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		if strings.HasPrefix(path, "~") {
			return "", fmt.Errorf("writable path %q uses unsupported home expansion", path)
		}

		return path, nil
	}

	home, err := coagenthome.UserHome()
	if err != nil {
		return "", fmt.Errorf("get home directory: %w", err)
	}

	if path == "~" {
		return home, nil
	}

	return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
}

func existingUserCacheDir() (string, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("get user cache directory: %w", err)
	}

	info, err := os.Stat(cacheDir)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}

	if err != nil {
		return "", fmt.Errorf("stat user cache directory: %w", err)
	}

	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", cacheDir)
	}

	return cacheDir, nil
}

func preflight(runner Runner, workDir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), preflightTimeout)
	defer cancel()

	cmd, err := runner.BashCommand(ctx, ":", workDir)
	if err != nil {
		return fmt.Errorf("construct preflight command: %w", err)
	}

	var output limitedBuffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	cmd.WaitDelay = preflightTimeout

	err = cmd.Run()
	if err == nil {
		return nil
	}

	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("preflight timed out after %s", preflightTimeout)
	}

	detail := strings.TrimSpace(output.String())
	if detail == "" {
		return fmt.Errorf("preflight command: %w", err)
	}

	return fmt.Errorf("preflight command: %w: %s", err, detail)
}

func probeEnforcement(newRunner runnerFactory) error {
	base, err := os.MkdirTemp("", "coagent-sandbox-probe-")
	if err != nil {
		return fmt.Errorf("create probe directory: %w", err)
	}

	defer func() { _ = os.RemoveAll(base) }()

	// The macOS temp root is a symlink; both backends match canonical paths.
	base, err = filepath.EvalSymlinks(base)
	if err != nil {
		return fmt.Errorf("resolve probe directory: %w", err)
	}

	allowed := filepath.Join(base, "allowed")
	denied := filepath.Join(base, "denied")

	if err := os.Mkdir(allowed, 0o700); err != nil {
		return fmt.Errorf("create allowed probe directory: %w", err)
	}

	if err := os.Mkdir(denied, 0o700); err != nil {
		return fmt.Errorf("create denied probe directory: %w", err)
	}

	runner, err := newRunner(processPolicy{
		readScope: HostReadable, workDir: allowed, projectRoot: allowed,
		writableRoots: []string{allowed}, sessionKey: "probe",
	})
	if err != nil {
		return fmt.Errorf("create probe runner: %w", err)
	}

	if err := probeAllowedWrite(runner, allowed); err != nil {
		return err
	}

	return probeDeniedWrite(runner, denied)
}

//nolint:wsl_v5 // The probe keeps allowed and denied observations in one auditable sequence.
func probeShieldEnforcement(newRunner runnerFactory) error {
	fixture, err := newShieldProbeFixture()
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(fixture.base) }()

	mounts, err := executionSubstrate()
	if err != nil {
		return err
	}
	runner, err := newRunner(processPolicy{
		readScope: ProjectConfined, workDir: fixture.project, projectRoot: fixture.project,
		writableRoots: []string{fixture.project}, readMounts: mounts, sessionKey: "probe-shields",
	})
	if err != nil {
		return fmt.Errorf("create shields probe runner: %w", err)
	}

	inside := filepath.Join(fixture.project, "inside")
	if output, err := runProbeIn(
		runner,
		"printf allowed >"+shellPath(inside)+" && cat "+shellPath(inside),
		fixture.project,
	); err != nil {
		return fmt.Errorf("shielded project access probe: %w: %s", err, output)
	}
	if output, err := runProbeIn(runner, "cat "+shellPath(fixture.secret), fixture.project); err == nil {
		return fmt.Errorf("shielded probe read denied path %q: %s", fixture.secret, output)
	}
	if output, err := runProbeIn(
		runner,
		"printf denied >"+shellPath(filepath.Join(fixture.denied, "write")),
		fixture.project,
	); err == nil {
		return fmt.Errorf("shielded probe wrote denied path %q: %s", fixture.denied, output)
	}

	return nil
}

//nolint:wsl_v5 // Fixture construction cleans each partial filesystem state before returning.
func newShieldProbeFixture() (shieldProbeFixture, error) {
	base, err := os.MkdirTemp("", "coagent-shields-probe-")
	if err != nil {
		return shieldProbeFixture{}, fmt.Errorf("create shields probe directory: %w", err)
	}
	base, err = filepath.EvalSymlinks(base)
	if err != nil {
		_ = os.RemoveAll(base)

		return shieldProbeFixture{}, fmt.Errorf("resolve shields probe directory: %w", err)
	}
	fixture := shieldProbeFixture{
		base: base, project: filepath.Join(base, "project"), denied: filepath.Join(base, "denied"),
	}
	fixture.secret = filepath.Join(fixture.denied, "secret")
	if err := os.Mkdir(fixture.project, 0o700); err != nil {
		_ = os.RemoveAll(base)

		return shieldProbeFixture{}, fmt.Errorf("create shields project: %w", err)
	}
	if err := os.Mkdir(fixture.denied, 0o700); err != nil {
		_ = os.RemoveAll(base)

		return shieldProbeFixture{}, fmt.Errorf("create shields denied directory: %w", err)
	}
	if err := os.WriteFile(fixture.secret, []byte("secret"), 0o600); err != nil {
		_ = os.RemoveAll(base)

		return shieldProbeFixture{}, fmt.Errorf("write shields denied probe: %w", err)
	}

	return fixture, nil
}

func probeAllowedWrite(runner Runner, dir string) error {
	path := filepath.Join(dir, "probe")

	output, err := runProbe(runner, "printf allowed >"+shellPath(path))
	if err != nil {
		return fmt.Errorf("write allowed probe: %w: %s", err, output)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("verify allowed probe: %w", err)
	}

	if !bytes.Equal(content, []byte("allowed")) {
		return fmt.Errorf("verify allowed probe: unexpected content %q", content)
	}

	return nil
}

func probeDeniedWrite(runner Runner, dir string) error {
	path := filepath.Join(dir, "probe")

	output, err := runProbe(runner, "printf denied >"+shellPath(path))
	if err == nil {
		return fmt.Errorf("sandbox allowed write to denied probe path %q", path)
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("denied-write probe timed out: %w", err)
	}

	_, err = os.Stat(path)
	if err == nil {
		return fmt.Errorf("sandbox modified denied probe path %q: %s", path, output)
	}

	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect denied probe path %q: %w", path, err)
	}

	return nil
}

func runProbe(runner Runner, command string) (string, error) {
	return runProbeIn(runner, command, "/")
}

func runProbeIn(runner Runner, command, workDir string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), preflightTimeout)
	defer cancel()

	cmd, err := runner.BashCommand(ctx, command, workDir)
	if err != nil {
		return "", fmt.Errorf("construct probe command: %w", err)
	}

	var output limitedBuffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	cmd.WaitDelay = preflightTimeout

	err = cmd.Run()
	detail := strings.TrimSpace(output.String())

	if ctx.Err() != nil {
		return detail, ctx.Err()
	}

	if err != nil {
		return detail, fmt.Errorf("run probe command: %w", err)
	}

	return detail, nil
}

func shellPath(path string) string {
	return "'" + strings.ReplaceAll(path, "'", "'\\''") + "'"
}

func policyKey(roots []string, sessionKey string) string {
	hash := sha256.Sum256([]byte(strings.Join(roots, "\x00") + "\x1f" + sessionKey))

	return hex.EncodeToString(hash[:])
}
