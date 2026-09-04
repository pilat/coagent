package bashsandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/shellenv"
)

const (
	preflightOutputLimit = 8 * 1024
	preflightTimeout     = 3 * time.Second
)

// enforcement caches the backend confinement probe: it spawns sandboxed
// processes and its outcome cannot change while the process runs.
var enforcement struct {
	once sync.Once
	err  error
}

// Config configures Bash filesystem write confinement.
type Config struct {
	Enabled       bool
	WorkDir       string
	WritablePaths []string
	ReadOnlyPaths []string // paths to mount read-only (for worktree git access)
}

// Runner constructs Bash commands with the configured sandbox policy.
type Runner interface {
	// Command builds a plain `bash -c` command that never sources a shell-env
	// snapshot. Internal helpers only (file mutation, probes) — use ShellCommand.
	Command(ctx context.Context, command, workDir string, args ...string) (*exec.Cmd, error)

	// ShellCommand builds a user shell command, sourcing workDir's shell-env
	// snapshot when one is available so the command sees the project's activated
	// toolchain (mise/asdf/etc.). Falls back to plain `bash -c` otherwise.
	ShellCommand(ctx context.Context, command, workDir string) (*exec.Cmd, error)

	// WritableRoots reports the normalized filesystem roots the sandbox allows
	// writes under; nil when confinement is disabled.
	WritableRoots() []string
}

var _ Runner = disabledRunner{}

// providerAware lets New attach the shell-env provider to a platform runner
// built by the provider-less runnerFactory, so probe runners never snapshot.
type providerAware interface {
	setProvider(p shellenv.Provider)
}

type runnerFactory func(writableRoots, readOnlyRoots []string) (Runner, error)

type limitedBuffer struct {
	buffer bytes.Buffer
}

type disabledRunner struct {
	provider shellenv.Provider
}

// New constructs a Bash command runner. provider may be nil: ShellCommand then
// degrades to plain `bash -c`.
func New(cfg Config, provider shellenv.Provider) (Runner, error) {
	if !cfg.Enabled {
		return disabledRunner{provider: provider}, nil
	}

	paths := make([]string, 0, len(cfg.WritablePaths)+4)
	paths = append(paths, cfg.WorkDir, os.TempDir(), "/tmp")

	cacheDir, err := existingUserCacheDir()
	if err != nil {
		return nil, fmt.Errorf("resolve user cache directory: %w", err)
	}

	if cacheDir != "" {
		paths = append(paths, cacheDir)
	}

	paths = append(paths, cfg.WritablePaths...)

	writableRoots, err := normalizeWritableRoots(paths)
	if err != nil {
		return nil, fmt.Errorf("normalize Bash sandbox writable roots: %w", err)
	}

	var readOnlyRoots []string
	if len(cfg.ReadOnlyPaths) > 0 {
		readOnlyRoots, err = normalizeWritableRoots(cfg.ReadOnlyPaths)
		if err != nil {
			return nil, fmt.Errorf("normalize Bash sandbox read-only roots: %w", err)
		}
	}

	if err := Probe(); err != nil {
		return nil, err
	}

	runner, err := newEnabledRunner(writableRoots, readOnlyRoots)
	if err != nil {
		return nil, fmt.Errorf("create Bash sandbox runner: %w", err)
	}

	if aware, ok := runner.(providerAware); ok {
		aware.setProvider(provider)
	}

	return runner, nil
}

// Probe verifies that the platform backend actually confines writes: a write
// inside a writable root must succeed, one outside must fail and leave no file.
// It runs at most once per process — New calls it lazily, main calls it at
// startup so a backend that cannot enforce fails the daemon rather than the
// first session.
func Probe() error {
	enforcement.once.Do(func() {
		enforcement.err = probeEnforcement(newEnabledRunner)
	})

	return enforcement.err
}

func (disabledRunner) Command(
	ctx context.Context,
	command, workDir string,
	args ...string,
) (*exec.Cmd, error) {
	commandArgs := append([]string{"-c", command}, args...)
	cmd := exec.CommandContext(ctx, "bash", commandArgs...)
	cmd.Dir = workDir

	return cmd, nil
}

func (r disabledRunner) ShellCommand(ctx context.Context, command, workDir string) (*exec.Cmd, error) {
	shell, snap := snapshotFor(ctx, r.provider, workDir)
	if snap == "" {
		cmd := exec.CommandContext(ctx, "bash", "-c", command)
		cmd.Dir = workDir

		return cmd, nil
	}

	cmd := exec.CommandContext(ctx, shell, "-c", sourceLine(snap, command))
	cmd.Dir = workDir

	return cmd, nil
}

func (disabledRunner) WritableRoots() []string { return nil }

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

func preflight(runner Runner) error {
	ctx, cancel := context.WithTimeout(context.Background(), preflightTimeout)
	defer cancel()

	cmd, err := runner.Command(ctx, ":", "/")
	if err != nil {
		return fmt.Errorf("construct preflight command: %w", err)
	}

	var output limitedBuffer
	cmd.Stdout = &output
	cmd.Stderr = &output

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

	runner, err := newRunner([]string{allowed}, nil)
	if err != nil {
		return fmt.Errorf("create probe runner: %w", err)
	}

	if err := probeAllowedWrite(runner, allowed); err != nil {
		return err
	}

	return probeDeniedWrite(runner, denied)
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
	ctx, cancel := context.WithTimeout(context.Background(), preflightTimeout)
	defer cancel()

	cmd, err := runner.Command(ctx, command, "/")
	if err != nil {
		return "", fmt.Errorf("construct probe command: %w", err)
	}

	var output limitedBuffer
	cmd.Stdout = &output
	cmd.Stderr = &output

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
