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

	cmd, err := runner.Command(
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
	restore := coagenthome.Override(home)
	defer restore()

	expanded, err := expandHome("~/sandbox-cache")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "sandbox-cache"), expanded)

	expanded, err = expandHome("~")
	require.NoError(t, err)
	assert.Equal(t, home, expanded)
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
}

func TestNew_RejectsDangerousTempRoot(t *testing.T) {
	workDir := t.TempDir()
	t.Setenv("TMPDIR", string(os.PathSeparator))

	_, err := New(Config{Enabled: true, WorkDir: workDir}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resolves to filesystem root")
}

func TestPreflight_PropagatesCommandConstructionError(t *testing.T) {
	want := errors.New("no command")
	err := preflight(errorRunner{err: want})
	require.Error(t, err)
	assert.ErrorIs(t, err, want)
}

func TestPreflight_BoundsLauncherOutput(t *testing.T) {
	err := preflight(noisyRunner{})
	require.Error(t, err)
	assert.LessOrEqual(t, len(err.Error()), preflightOutputLimit+100)
}

func TestProbeEnforcement_RejectsBackendThatRunsNothing(t *testing.T) {
	err := probeEnforcement(func([]string) (Runner, error) {
		return noopRunner{}, nil
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "verify allowed probe")
}

func TestProbeEnforcement_RejectsBackendThatDoesNotConfine(t *testing.T) {
	err := probeEnforcement(func([]string) (Runner, error) {
		return disabledRunner{}, nil
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sandbox allowed write to denied probe path")
}

func TestProbeEnforcement_PropagatesBackendConstructionError(t *testing.T) {
	want := errors.New("no backend")

	err := probeEnforcement(func([]string) (Runner, error) {
		return nil, want
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, want)
}

func (r errorRunner) Command(context.Context, string, string, ...string) (*exec.Cmd, error) {
	return nil, r.err
}

func (errorRunner) WritableRoots() []string { return nil }

func (r errorRunner) ShellCommand(context.Context, string, string) (*exec.Cmd, error) {
	return nil, r.err
}

func (noopRunner) Command(ctx context.Context, _, _ string, _ ...string) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, "bash", "-c", ":"), nil
}

func (noopRunner) WritableRoots() []string { return nil }

func (noopRunner) ShellCommand(ctx context.Context, _, _ string) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, "bash", "-c", ":"), nil
}

func (noisyRunner) Command(ctx context.Context, _, _ string, _ ...string) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, "bash", "-c", "printf '%0100000d' 0 >&2; exit 1"), nil
}

func (noisyRunner) WritableRoots() []string { return nil }

func (noisyRunner) ShellCommand(ctx context.Context, _, _ string) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, "bash", "-c", "printf '%0100000d' 0 >&2; exit 1"), nil
}
