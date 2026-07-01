package builtin

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fileMutationCall struct {
	ctx           context.Context
	path          string
	content       []byte
	createParents bool
}

type recordingFileMutator struct {
	calls []fileMutationCall
	err   error
}

type fixedCommandRunner struct {
	command string
	err     error
}

type mutationContextKey struct{}

func (m *recordingFileMutator) WriteFile(
	ctx context.Context,
	path string,
	content []byte,
	createParents bool,
) error {
	m.calls = append(m.calls, fileMutationCall{
		ctx:           ctx,
		path:          path,
		content:       append([]byte(nil), content...),
		createParents: createParents,
	})

	return m.err
}

func (r fixedCommandRunner) Command(
	ctx context.Context,
	_, workDir string,
	_ ...string,
) (*exec.Cmd, error) {
	if r.err != nil {
		return nil, r.err
	}

	cmd := exec.CommandContext(ctx, "bash", "-c", r.command)
	cmd.Dir = workDir

	return cmd, nil
}

// ShellCommand exists only to satisfy Runner; the file mutator uses Command.
func (r fixedCommandRunner) ShellCommand(ctx context.Context, command, workDir string) (*exec.Cmd, error) {
	return r.Command(ctx, command, workDir)
}

func (fixedCommandRunner) WritableRoots() []string { return nil }

// methodSpyRunner records whether the snapshot-sourcing ShellCommand path is ever
// taken. Command runs a real bash so the mutation actually writes.
type methodSpyRunner struct {
	shellCommandCalled bool
}

func (r *methodSpyRunner) Command(ctx context.Context, command, workDir string, args ...string) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, "bash", append([]string{"-c", command}, args...)...), nil
}

func (r *methodSpyRunner) ShellCommand(ctx context.Context, command, _ string) (*exec.Cmd, error) {
	r.shellCommandCalled = true
	return exec.CommandContext(ctx, "bash", "-c", command), nil
}

func (*methodSpyRunner) WritableRoots() []string { return nil }

// TestSandboxFileMutator_NeverSourcesSnapshot guards the write-safety seam: file
// mutations must go through plain Command, never ShellCommand — so a user alias
// or function in the snapshot (e.g. shadowing cat/mkdir) can never corrupt a write.
func TestSandboxFileMutator_NeverSourcesSnapshot(t *testing.T) {
	spy := &methodSpyRunner{}

	mutator, err := newFileMutator(true, spy)
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "nested", "file.txt")
	require.NoError(t, mutator.WriteFile(context.Background(), path, []byte("payload"), true))

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "payload", string(content))
	assert.False(t, spy.shellCommandCalled, "file mutation must use Command, never the snapshot-sourcing ShellCommand")
}

func TestDirectFileMutator(t *testing.T) {
	mutator, err := newFileMutator(false, nil)
	require.NoError(t, err)

	t.Run("creates parents and replaces content", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "nested", "file.txt")
		require.NoError(t, mutator.WriteFile(context.Background(), path, []byte("first"), true))
		require.NoError(t, mutator.WriteFile(context.Background(), path, []byte("second"), false))

		content, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, "second", string(content))
	})

	t.Run("does not create parents when disabled for the call", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "missing")
		path := filepath.Join(parent, "file.txt")

		err := mutator.WriteFile(context.Background(), path, []byte("content"), false)
		require.Error(t, err)
		assert.NoDirExists(t, parent)
	})
}

func TestSandboxFileMutator_UsesPositionalPathsAndStdin(t *testing.T) {
	runner := &bashRunnerStub{}
	mutator, err := newFileMutator(true, runner)
	require.NoError(t, err)

	parent := filepath.Join(t.TempDir(), "odd ' ; $() ` parent")
	path := filepath.Join(parent, "line\nbreak.txt")
	content := []byte("content $(touch nope); `still data`\n")

	require.NoError(t, mutator.WriteFile(context.Background(), path, content, true))

	actual, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, content, actual)
	assert.Equal(t, sandboxMutationCommand, runner.command)
	assert.NotContains(t, runner.command, path)
	assert.NotContains(t, runner.command, string(content))
	assert.Equal(t, []string{"coagent-file-mutator", path, "1", parent}, runner.args)
}

func TestSandboxFileMutator_CommandConstructionFailsClosed(t *testing.T) {
	want := errors.New("runner unavailable")
	runner := &bashRunnerStub{err: want}
	mutator, err := newFileMutator(true, runner)
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "missing.txt")
	err = mutator.WriteFile(context.Background(), path, []byte("content"), true)

	require.ErrorIs(t, err, want)
	assert.NoFileExists(t, path)
}

func TestSandboxFileMutator_BoundsHelperOutput(t *testing.T) {
	mutator, err := newFileMutator(true, fixedCommandRunner{
		command: "printf '%0100000d' 0 >&2; exit 1",
	})
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "sentinel.txt")
	require.NoError(t, os.WriteFile(path, []byte("before"), 0o644))

	err = mutator.WriteFile(context.Background(), path, []byte("after"), false)

	require.Error(t, err)
	assert.LessOrEqual(t, len(err.Error()), fileMutationOutputLimit+150)
	assert.Contains(t, err.Error(), "output truncated")
	content, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, "before", string(content))
}

func TestSandboxFileMutator_HelperFailureFailsClosed(t *testing.T) {
	mutator, err := newFileMutator(true, fixedCommandRunner{command: "printf failed >&2; exit 1"})
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "sentinel.txt")

	err = mutator.WriteFile(context.Background(), path, []byte("after"), false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed")
	assert.NoFileExists(t, path)
}

func TestSandboxFileMutator_PropagatesCancellation(t *testing.T) {
	mutator, err := newFileMutator(true, fixedCommandRunner{command: "sleep 10"})
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "sentinel.txt")
	require.NoError(t, os.WriteFile(path, []byte("before"), 0o644))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	started := time.Now()
	err = mutator.WriteFile(ctx, path, []byte("after"), false)

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(started), 2*time.Second)
	content, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, "before", string(content))
}

func TestNewFileMutator_EnabledRequiresRunner(t *testing.T) {
	mutator, err := newFileMutator(true, nil)

	require.Error(t, err)
	assert.Nil(t, mutator)
	assert.Contains(t, err.Error(), "runner")
}
