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
	"github.com/pilat/coagent/internal/sandboxpolicy"
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

func (f fakeProvider) Snapshot(context.Context, shellenv.ConfinedRunner, string) string {
	return f.snap
}
func (f fakeProvider) Shell() string           { return f.shell }
func (fakeProvider) Fingerprint(string) string { return "" }
func (fakeProvider) Invalidate(string)         {}
func (f fakeProvider) Close() error            { return nil }

func (fakeProvider) WrapExec(context.Context, shellenv.ConfinedRunner, string, []string, []string) (*exec.Cmd, error) {
	return nil, nil
}

func (fakeProvider) LookPath(context.Context, shellenv.ConfinedRunner, string, []string) (string, error) {
	return "", os.ErrNotExist
}

type errorRunner struct {
	err error
}

type noisyRunner struct{}

// noopRunner stands in for a backend that accepts the command and silently
// runs nothing.
type noopRunner struct{}

// testSandboxHome isolates HOME and the environment names the shipped catalog
// resolves, so catalog paths are deterministic and the real coagent home is
// never touched.
func testSandboxHome(t *testing.T) string {
	t.Helper()

	home := t.TempDir()
	t.Cleanup(coagenthome.Override(home))
	t.Setenv("HOME", home)

	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Setenv("GH_CONFIG_DIR", filepath.Join(home, ".config", "gh"))
	t.Setenv("SSH_AUTH_SOCK", filepath.Join(home, "agent.sock"))

	return home
}

func testDir(t *testing.T, path string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(path, 0o700))

	return path
}

// testPolicyRequest compiles the base authority for one test project.
func testPolicyRequest(t *testing.T, project string) sandboxpolicy.Request {
	t.Helper()

	return testPolicyRequestShields(t, project, false)
}

// testShieldedPolicyRequest compiles the raised-shields authority.
func testShieldedPolicyRequest(t *testing.T, project string) sandboxpolicy.Request {
	t.Helper()

	return testPolicyRequestShields(t, project, true)
}

func testPolicyRequestShields(t *testing.T, project string, shields bool) sandboxpolicy.Request {
	t.Helper()

	substrate, err := ExecutionSubstrate()
	require.NoError(t, err)

	tempRoot, err := coagenthome.SandboxTempDir(coagenthome.SandboxPathIdentity(project))
	require.NoError(t, err)

	return sandboxpolicy.Request{
		ProjectRoot: project, ProjectID: 1, WorkDir: project,
		TempRoot: tempRoot, Shields: shields, Substrate: substrate,
	}
}

func testCatalog(t *testing.T, overrides map[string]sandboxpolicy.Profile) sandboxpolicy.Catalog {
	t.Helper()

	catalog, err := sandboxpolicy.Load(overrides)
	require.NoError(t, err)

	return catalog
}

// testCompiledPolicy compiles the default base policy for a project.
func testCompiledPolicy(t *testing.T, project string, shields bool) sandboxpolicy.Policy {
	t.Helper()

	compiled, err := sandboxpolicy.Compile(
		testCatalog(t, nil), testPolicyRequestShields(t, project, shields),
	)
	require.NoError(t, err)

	return compiled
}

// testProcessPolicy derives the process policy from a compiled one.
func testProcessPolicy(t *testing.T, compiled sandboxpolicy.Policy, sessionKeys ...string) processPolicy {
	t.Helper()

	sessionKey := "session:1"
	if len(sessionKeys) > 0 {
		sessionKey = sessionKeys[0]
	}

	policy, err := buildProcessPolicy(Config{
		Enabled: true, Shields: compiled.Shields, Policy: compiled,
		WorkDir: compiled.WorkDir, SessionKey: sessionKey,
	})
	require.NoError(t, err)

	return policy
}

func TestNew_DisabledPreservesCommand(t *testing.T) {
	runner, err := New(Config{Enabled: false}, nil)
	require.NoError(t, err)

	assert.NotEmpty(t, runner.PolicyKey())
	assert.Equal(t, HostReadable, runner.ReadScope())
	assert.Empty(t, runner.WritableRoots())
}

func TestNew_DisabledShellCommandDegradesWithoutProvider(t *testing.T) {
	runner, err := New(Config{Enabled: false, SessionKey: "session:1"}, nil)
	require.NoError(t, err)

	cmd, err := runner.ShellCommand(t.Context(), "go version", "/work dir")
	require.NoError(t, err)
	assert.Equal(t, []string{"bash", "-c", "go version"}, cmd.Args)
	assert.Equal(t, "/work dir", cmd.Dir)
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

func TestDisabledRunnerCommandAppliesRequest(t *testing.T) {
	runner := disabledRunner{}

	cmd, err := runner.Command(context.Background(), procexec.Request{
		Path: "/bin/echo", Args: []string{"hi"}, WorkDir: "/work", Env: []string{"A=b"},
	})
	require.NoError(t, err)
	assert.Equal(t, "/bin/echo", cmd.Path)
	assert.Equal(t, []string{"/bin/echo", "hi"}, cmd.Args)
	assert.Equal(t, "/work", cmd.Dir)
	assert.Equal(t, []string{"A=b"}, cmd.Env)
}

func (r errorRunner) Command(context.Context, procexec.Request) (*exec.Cmd, error) {
	return nil, r.err
}

func (r errorRunner) BashCommand(context.Context, string, string, ...string) (*exec.Cmd, error) {
	return nil, r.err
}

func (errorRunner) PolicyKey() string { return "error" }

func (errorRunner) WritableRoots() []string { return nil }
func (errorRunner) ProjectRoot() string     { return "" }
func (errorRunner) ReadScope() ReadScope    { return HostReadable }
func (errorRunner) AllowsRead(string) bool  { return true }

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
func (noopRunner) ProjectRoot() string     { return "" }
func (noopRunner) ReadScope() ReadScope    { return HostReadable }
func (noopRunner) AllowsRead(string) bool  { return true }

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
func (noisyRunner) ProjectRoot() string     { return "" }
func (noisyRunner) ReadScope() ReadScope    { return HostReadable }
func (noisyRunner) AllowsRead(string) bool  { return true }

func (noisyRunner) ShellCommand(ctx context.Context, _, _ string) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, "bash", "-c", "printf '%0100000d' 0 >&2; exit 1"), nil
}
