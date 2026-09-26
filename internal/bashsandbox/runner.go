package bashsandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/pilat/coagent/internal/procexec"
	"github.com/pilat/coagent/internal/sandboxpolicy"
	"github.com/pilat/coagent/internal/shellenv"
)

const (
	bashExecutable       = "bash"
	devPath              = "/dev"
	preflightOutputLimit = 8 * 1024
	preflightTimeout     = 3 * time.Second

	// snapshotRuntimeDir is the private in-sandbox path a shell-env snapshot is
	// exposed at. Only the owning snapshot is mounted there, never the cache.
	snapshotRuntimeDir = "/run/coagent/shellenv"
)

// enforcement caches the backend confinement probe: it spawns sandboxed
// processes and its outcome cannot change while the process runs.
var enforcement struct {
	once sync.Once
	err  error
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

	// WritableRoots reports the read-write grant targets, project first.
	WritableRoots() []string

	// ProjectRoot reports the session's canonical project root; empty when the
	// sandbox is disabled. Callers use it instead of the first writable root.
	ProjectRoot() string

	// ReadScope reports the authority class the runner enforces.
	ReadScope() ReadScope

	// AllowsRead reports whether the compiled policy admits reading path.
	AllowsRead(path string) bool
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

type environmentEntry struct {
	name  string
	value string
}

type disabledRunner struct {
	provider  shellenv.Provider
	policyKey string
}

// New constructs a Bash command runner. provider may be nil: ShellCommand then
// degrades to plain `bash -c`.
func New(cfg Config, provider shellenv.Provider) (Runner, error) {
	if !cfg.Enabled {
		return disabledRunner{provider: provider, policyKey: policyKey("disabled", cfg.SessionKey)}, nil
	}

	policy, err := buildProcessPolicy(cfg)
	if err != nil {
		return nil, err
	}

	if err := Probe(); err != nil {
		return nil, err
	}

	runner, err := newEnabledRunnerWithNetwork(policy, cfg.Network)
	if err != nil {
		return nil, fmt.Errorf("create Bash sandbox runner: %w", err)
	}

	if aware, ok := runner.(providerAware); ok {
		aware.setProvider(provider)
	}

	return runner, nil
}

// Probe verifies that the platform backend actually confines processes: a write
// inside a granted root must succeed, one outside must fail and leave no file.
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

	return procexec.Unprivileged(cmd), nil
}

func (r disabledRunner) BashCommand(ctx context.Context, command, workDir string, args ...string) (*exec.Cmd, error) {
	return r.Command(ctx, procexec.Request{
		Path:    bashExecutable,
		Args:    append([]string{"-c", command}, args...),
		WorkDir: workDir,
	})
}

func (r disabledRunner) ShellCommand(ctx context.Context, command, workDir string) (*exec.Cmd, error) {
	shell, snap := snapshotFor(ctx, r.provider, r, workDir)
	if snap == "" {
		return r.BashCommand(ctx, command, workDir)
	}

	return r.SnapshotShell(ctx, shell, snap, command, workDir, nil)
}

// PolicyDigest reports the disabled policy, which owns its own snapshot key space.
func (r disabledRunner) PolicyDigest() string { return "disabled" }

// AllowsRead reports the unconfined truth: nothing is withheld.
func (r disabledRunner) AllowsRead(string) bool { return true }

// SnapshotShell runs the command on the host: with the sandbox disabled there is
// no mount to arrange and no authority to add.
func (r disabledRunner) SnapshotShell(
	ctx context.Context,
	shell, snapshot, command, workDir string,
	env []string,
) (*exec.Cmd, error) {
	line := command
	if snapshot != "" {
		line = sourceLine(snapshot, command)
	}

	return r.Command(ctx, procexec.Request{
		Path:    shell,
		Args:    []string{"-c", line},
		WorkDir: workDir,
		Env:     env,
	})
}

func (disabledRunner) WritableRoots() []string { return nil }
func (disabledRunner) ProjectRoot() string     { return "" }
func (r disabledRunner) PolicyKey() string     { return r.policyKey }
func (disabledRunner) ReadScope() ReadScope    { return HostReadable }

// snapshotFor resolves the shell and snapshot path for workDir, or ("", "") when
// snapshotting is unavailable (nil provider or graceful degradation). The runner
// is the seam capture executes through, so an rc file cannot act outside the
// policy while the snapshot is taken.
func snapshotFor(
	ctx context.Context,
	provider shellenv.Provider,
	runner shellenv.ConfinedRunner,
	workDir string,
) (string, string) {
	if provider == nil {
		return "", ""
	}

	snap := provider.Snapshot(ctx, runner, workDir)
	if snap == "" {
		return "", ""
	}

	return provider.Shell(), snap
}

// snapshotRuntimePath is the private path a snapshot file appears at inside the
// sandbox. The digest keeps two snapshots from colliding without exposing the
// host cache layout.
func snapshotRuntimePath(snapshot string) string {
	hash := sha256.Sum256([]byte(snapshot))

	return snapshotRuntimeDir + "/" + hex.EncodeToString(hash[:8]) + "/snapshot"
}

// snapshotGrant mounts one snapshot file read-only at its runtime path.
func snapshotGrant(snapshot string) []sandboxpolicy.Grant {
	return []sandboxpolicy.Grant{{
		Source: snapshot, Target: snapshotRuntimePath(snapshot), Mode: sandboxpolicy.ModeReadOnly,
		Kind: sandboxpolicy.KindFile, Present: true,
	}}
}

// sourceLine builds `source <snap>; <command>` with the snapshot path quoted.
func sourceLine(snap, command string) string {
	return "source " + shellPath(snap) + "; " + command
}

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

		switch name {
		case "PWD":
			value = workDir
			foundPWD = true
		case "TMPDIR", "TMP", "TEMP":
			// The private temporary storage is the only writable temp inside the
			// sandbox; a host temp path would point at an ungranted directory.
			value = sandboxpolicy.TempPath
		}

		entries = append(entries, environmentEntry{name: name, value: value})
	}

	if !foundPWD {
		entries = append(entries, environmentEntry{name: "PWD", value: workDir})
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

func preflight(runner Runner, workDir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), preflightTimeout)
	defer cancel()

	cmd, err := runner.BashCommand(ctx, ":", workDir)
	if err != nil {
		return fmt.Errorf("construct preflight command: %w", err)
	}
	defer procexec.CloseExtraFiles(cmd)

	var output limitedBuffer

	cmd.Stdout = &output
	cmd.Stderr = &output
	cmd.WaitDelay = preflightTimeout

	err = cmd.Run()
	if err == nil {
		return nil
	}

	if ctx.Err() != nil {
		return fmt.Errorf("preflight timed out after %s", preflightTimeout)
	}

	detail := strings.TrimSpace(output.String())
	if detail == "" {
		return fmt.Errorf("preflight command: %w", err)
	}

	return fmt.Errorf("preflight command: %w: %s", err, detail)
}

func shellPath(path string) string {
	return "'" + strings.ReplaceAll(path, "'", "'\\''") + "'"
}

func policyKey(digest, sessionKey string) string {
	hash := sha256.Sum256([]byte(digest + "\x1f" + sessionKey))

	return hex.EncodeToString(hash[:])
}
