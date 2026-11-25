package builtin

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/todo"
)

var hookSubreaperProcess = flag.Bool("hook-test-subreaper", false, "run the isolated hook cancellation fixture")

func TestRunProcessClosesExtraFiles(t *testing.T) {
	tests := []struct {
		name     string
		command  string
		args     []string
		wantCode int
		wantErr  bool
	}{
		{
			name:     "success",
			command:  "bash",
			args:     []string{"-c", "exit 0"},
			wantCode: 0,
		},
		{
			name:     "nonzero",
			command:  "bash",
			args:     []string{"-c", "exit 7"},
			wantCode: 7,
		},
		{
			name:    "failedStart",
			command: filepath.Join(t.TempDir(), "missing"),
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), tt.command, tt.args...)
			assertRunProcessExtraFilesClosed(t, cmd, tt.wantCode, tt.wantErr)
		})
	}
}

func TestRunProcessCapsOutputAndReturnsExitCode(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "bash", "-c",
		`read -r value; printf '%s' "$value"; printf '%70000s' a; printf '%70000s' b >&2; exit 7`)
	output, err := runProcess(cmd, []byte("payload\n"))
	require.NoError(t, err)
	assert.Equal(t, 7, output.ExitCode)
	assert.Len(t, output.Stdout, 64*1024)
	assert.Len(t, output.Stderr, 64*1024)
	assert.True(t, strings.HasPrefix(string(output.Stdout), "payload"))
}

func TestRunProcessCancellationKillsDescendants(t *testing.T) {
	if !*hookSubreaperProcess {
		runHookSubreaperFixture(t)
		return
	}

	require.NoError(t, unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0))

	workDir := t.TempDir()
	stack, err := BuildStack(t.Context(), StackConfig{
		WorkDir: workDir,
		Loader:  loader.New(),
		Todo:    todo.New(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = stack.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	finished := make(chan error, 1)

	go func() {
		_, runErr := stack.RunProcess(ctx, ProcessRun{
			Argv: []string{"bash", "-c", `sleep 30 & child=$!; printf '%s\n' "$child" > child.pid; wait "$child"`},
		})
		finished <- runErr
	}()

	pid := waitHookDescendantPID(t, filepath.Join(workDir, "child.pid"))
	t.Cleanup(func() {
		if !reapHookDescendant(pid) {
			_ = unix.Kill(pid, unix.SIGKILL)
			assert.Eventually(t, func() bool { return reapHookDescendant(pid) }, 3*time.Second, 10*time.Millisecond)
		}
	})
	cancel()

	select {
	case err := <-finished:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(7 * time.Second):
		t.Fatal("hook process did not finish after cancellation")
	}

	assert.Eventually(t, func() bool { return reapHookDescendant(pid) }, time.Second, 10*time.Millisecond)
	assert.ErrorIs(t, unix.Kill(pid, 0), unix.ESRCH)
}

func runHookSubreaperFixture(t *testing.T) {
	t.Helper()

	// Isolate child reaping so the rest of the suite keeps its normal process ownership.
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	executable, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.CommandContext(ctx, executable,
		"-test.run=^TestRunProcessCancellationKillsDescendants$", "-hook-test-subreaper")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
}

func assertRunProcessExtraFilesClosed(t *testing.T, cmd *exec.Cmd, wantCode int, wantErr bool) {
	t.Helper()

	readEnd, writeEnd, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() { _ = readEnd.Close() })
	t.Cleanup(func() { _ = writeEnd.Close() })
	cmd.ExtraFiles = []*os.File{readEnd}
	output, err := runProcess(cmd, nil)
	if wantErr {
		require.Error(t, err)
	} else {
		require.NoError(t, err)
		assert.Equal(t, wantCode, output.ExitCode)
	}
	_, err = readEnd.Stat()
	assert.ErrorIs(t, err, os.ErrClosed)
}

func waitHookDescendantPID(t *testing.T, path string) int {
	t.Helper()

	var pid int
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(path)
		if err != nil {
			return false
		}

		pid, err = strconv.Atoi(strings.TrimSpace(string(data)))

		return err == nil && pid > 0
	}, 3*time.Second, 10*time.Millisecond)

	return pid
}

func reapHookDescendant(pid int) bool {
	_, err := unix.Wait4(pid, nil, unix.WNOHANG, nil)
	if err != nil && !errors.Is(err, unix.ECHILD) {
		return false
	}

	return errors.Is(unix.Kill(pid, 0), unix.ESRCH)
}
