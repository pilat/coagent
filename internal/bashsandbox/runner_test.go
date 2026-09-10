package bashsandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/procexec"
	"github.com/pilat/coagent/internal/shellenv"
)

var (
	_ Runner            = errorRunner{}
	_ Runner            = noopRunner{}
	_ shellenv.Provider = fakeProvider{}
)

// fakeProvider feeds a fixed snapshot into a runner without spawning a shell.
type fakeProvider struct {
	shell string
	snap  string
}

func (f fakeProvider) Snapshot(context.Context, string) string { return f.snap }
func (f fakeProvider) Shell() string                           { return f.shell }
func (fakeProvider) Fingerprint(string) string                 { return "" }
func (fakeProvider) Invalidate(string)                         {}
func (f fakeProvider) Close() error                            { return nil }

func (fakeProvider) WrapExec(context.Context, string, []string, []string) (*exec.Cmd, error) {
	return nil, nil
}

func (fakeProvider) LookPath(context.Context, string, []string) (string, error) {
	return "", os.ErrNotExist
}

type errorRunner struct {
	err error
}

type noisyRunner struct{}

// noopRunner stands in for a backend that accepts the command and silently
// runs nothing.
type noopRunner struct{}

func TestNew_DisabledPreservesCommand(t *testing.T) {
	t.Setenv("TMPDIR", string(os.PathSeparator))

	runner, err := New(Config{
		Enabled:       false,
		WorkDir:       "relative-does-not-matter",
		WritablePaths: []string{"missing-does-not-matter"},
	}, nil)
	require.NoError(t, err)

	cmd, err := runner.BashCommand(
		context.Background(),
		"printf '%s' \"$1\"",
		"/chosen/workdir",
		"coagent-test",
		"hello world",
	)
	require.NoError(t, err)

	assert.Equal(t, []string{
		"bash", "-c", "printf '%s' \"$1\"", "coagent-test", "hello world",
	}, cmd.Args)
	assert.Equal(t, "/chosen/workdir", cmd.Dir)
}

func TestNew_DisabledPolicyKeyIncludesSessionIdentity(t *testing.T) {
	first, err := New(Config{SessionKey: "session:1"}, nil)
	require.NoError(t, err)
	second, err := New(Config{SessionKey: "session:2"}, nil)
	require.NoError(t, err)
	repeat, err := New(Config{SessionKey: "session:1"}, nil)
	require.NoError(t, err)

	assert.NotEqual(t, first.PolicyKey(), second.PolicyKey())
	assert.Equal(t, first.PolicyKey(), repeat.PolicyKey())
}

func TestDisabledRunner_ShellCommandSourcesSnapshot(t *testing.T) {
	runner := disabledRunner{provider: fakeProvider{shell: "/bin/bash", snap: "/tmp/snap dir/s"}}

	cmd, err := runner.ShellCommand(context.Background(), "go version", "/work dir")
	require.NoError(t, err)

	assert.Equal(t, []string{"/bin/bash", "-c", "source '/tmp/snap dir/s'; go version"}, cmd.Args)
	assert.Equal(t, "/work dir", cmd.Dir)
}

func TestDisabledRunner_ShellCommandNoSnapshotFallsBackToBash(t *testing.T) {
	tests := map[string]disabledRunner{
		"nil provider":         {},
		"provider no snapshot": {provider: fakeProvider{shell: "/bin/bash", snap: ""}},
	}

	for name, runner := range tests {
		t.Run(name, func(t *testing.T) {
			cmd, err := runner.ShellCommand(context.Background(), "go version", "/work")
			require.NoError(t, err)

			assert.Equal(t, []string{"bash", "-c", "go version"}, cmd.Args)
			assert.Equal(t, "/work", cmd.Dir)
		})
	}
}

func TestNormalizeWritableRoots_CanonicalizesDeduplicatesAndOrders(t *testing.T) {
	parent := t.TempDir()
	child := filepath.Join(parent, "child")
	require.NoError(t, os.Mkdir(child, 0o755))

	alias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(child, alias))

	roots, err := normalizeWritableRoots([]string{child, alias, parent})
	require.NoError(t, err)

	resolvedParent, err := filepath.EvalSymlinks(parent)
	require.NoError(t, err)
	resolvedChild, err := filepath.EvalSymlinks(child)
	require.NoError(t, err)

	assert.Equal(t, []string{resolvedParent, resolvedChild}, roots)
}

func TestNormalizeWritableRoot_ExpandsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	require.NoError(t, os.MkdirAll(filepath.Join(home, "cache"), 0o700))
	restore := coagenthome.Override(home)
	defer restore()

	expanded, err := expandHome("~/sandbox-cache")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "sandbox-cache"), expanded)

	expanded, err = expandHome("~")
	require.NoError(t, err)
	assert.Equal(t, home, expanded)
}

func TestPreparePolicy_ProcessArtifactsFollowShieldBoundary(t *testing.T) {
	home := t.TempDir()
	restore := coagenthome.Override(home)
	defer restore()

	workDir := t.TempDir()
	ordinary, err := preparePolicy(Config{
		Enabled: true, ProjectID: 7, WorkDir: workDir, CanonicalWorkDir: workDir,
		SessionKey: "session:1", ReadScope: HostReadable,
	})
	require.NoError(t, err)
	processRoot, err := coagenthome.ProcessProjectDir(7)
	require.NoError(t, err)
	allProcessesRoot, err := coagenthome.Join(coagenthome.ProcessesDirName)
	require.NoError(t, err)
	processRoot, err = filepath.EvalSymlinks(processRoot)
	require.NoError(t, err)
	allProcessesRoot, err = filepath.EvalSymlinks(allProcessesRoot)
	require.NoError(t, err)
	foreignProcessRoot := filepath.Join(allProcessesRoot, "project-8")
	assert.Contains(t, ordinary.writableRoots, processRoot)
	assert.NotContains(t, ordinary.writableRoots, allProcessesRoot)
	assert.NotContains(t, ordinary.writableRoots, foreignProcessRoot)
	_, err = os.Stat(processRoot)
	require.NoError(t, err)
	assert.NotEqual(t,
		policyKey(ordinary.writableRoots, "ordinary"),
		policyKey(shieldedRootsWithout(processRoot, ordinary.writableRoots), "ordinary"),
	)

	shielded, err := preparePolicy(Config{
		Enabled: true, WorkDir: workDir, CanonicalWorkDir: workDir,
		SessionKey: "session:1", ReadScope: ProjectConfined,
		ExcludeSessionWritableRoots: true,
	})
	require.NoError(t, err)
	assert.NotContains(t, shielded.writableRoots, processRoot)
}

func shieldedRootsWithout(path string, roots []string) []string {
	filtered := make([]string, 0, len(roots))
	for _, root := range roots {
		if root != path {
			filtered = append(filtered, root)
		}
	}

	return filtered
}

func TestNormalizeWritableRoot_RejectsInvalidPaths(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, []byte("data"), 0o600))

	tests := map[string]struct {
		path    string
		message string
	}{
		"empty":           {path: "", message: "must be absolute"},
		"relative":        {path: "relative/path", message: "must be absolute"},
		"other user":      {path: "~someone/path", message: "unsupported home expansion"},
		"missing":         {path: filepath.Join(t.TempDir(), "missing"), message: "resolve writable path"},
		"regular file":    {path: file, message: "is not a directory"},
		"filesystem root": {path: string(os.PathSeparator), message: "resolves to filesystem root"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := normalizeWritableRoot(tt.path)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.message)
		})
	}

	if _, err := os.Stat("/proc"); err == nil {
		t.Run("proc", func(t *testing.T) {
			_, err := normalizeWritableRoot("/proc")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "cannot be under protected Linux root")
		})
	}
}

func TestNew_RejectsDangerousTempRoot(t *testing.T) {
	isolateCoagentHome(t)

	workDir := t.TempDir()
	t.Setenv("TMPDIR", string(os.PathSeparator))

	_, err := New(Config{Enabled: true, WorkDir: workDir}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resolves to filesystem root")
}

func isolateCoagentHome(t *testing.T) {
	t.Helper()

	restore := coagenthome.Override(t.TempDir())
	t.Cleanup(restore)
}

func TestPreflight_PropagatesCommandConstructionError(t *testing.T) {
	want := errors.New("no command")
	err := preflight(errorRunner{err: want}, "/")
	require.Error(t, err)
	assert.ErrorIs(t, err, want)
}

func TestPreflight_BoundsLauncherOutput(t *testing.T) {
	err := preflight(noisyRunner{}, "/")
	require.Error(t, err)
	assert.LessOrEqual(t, len(err.Error()), preflightOutputLimit+100)
}

func TestProbeEnforcement_RejectsBackendThatRunsNothing(t *testing.T) {
	err := probeEnforcement(func(processPolicy) (Runner, error) {
		return noopRunner{}, nil
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "verify allowed probe")
}

func TestProbeEnforcement_RejectsBackendThatDoesNotConfine(t *testing.T) {
	err := probeEnforcement(func(processPolicy) (Runner, error) {
		return disabledRunner{}, nil
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sandbox allowed write to denied probe path")
}

func TestProbeEnforcement_PropagatesBackendConstructionError(t *testing.T) {
	want := errors.New("no backend")

	err := probeEnforcement(func(processPolicy) (Runner, error) {
		return nil, want
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, want)
}

func (r errorRunner) Command(context.Context, procexec.Request) (*exec.Cmd, error) {
	return nil, r.err
}

func (r errorRunner) BashCommand(context.Context, string, string, ...string) (*exec.Cmd, error) {
	return nil, r.err
}

func (errorRunner) PolicyKey() string { return "error" }

func (errorRunner) WritableRoots() []string { return nil }
func (errorRunner) ReadScope() ReadScope    { return HostReadable }

func (r errorRunner) ShellCommand(context.Context, string, string) (*exec.Cmd, error) {
	return nil, r.err
}

func (noopRunner) Command(ctx context.Context, request procexec.Request) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, request.Path, request.Args...), nil
}

func (noopRunner) BashCommand(ctx context.Context, _, _ string, _ ...string) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, "bash", "-c", ":"), nil
}

func (noopRunner) PolicyKey() string { return "noop" }

func (noopRunner) WritableRoots() []string { return nil }
func (noopRunner) ReadScope() ReadScope    { return HostReadable }

func (noopRunner) ShellCommand(ctx context.Context, _, _ string) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, "bash", "-c", ":"), nil
}

func (noisyRunner) Command(ctx context.Context, request procexec.Request) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, "bash", "-c", "printf '%0100000d' 0 >&2; exit 1"), nil
}

func (noisyRunner) BashCommand(ctx context.Context, _, _ string, _ ...string) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, "bash", "-c", "printf '%0100000d' 0 >&2; exit 1"), nil
}

func (noisyRunner) PolicyKey() string { return "noisy" }

func (noisyRunner) WritableRoots() []string { return nil }
func (noisyRunner) ReadScope() ReadScope    { return HostReadable }

func (noisyRunner) ShellCommand(ctx context.Context, _, _ string) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, "bash", "-c", "printf '%0100000d' 0 >&2; exit 1"), nil
}
