package builtin

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/tool"
)

func TestApplyPatchHookCallsRejectsFIFO(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	require.NoError(t, syscall.Mkfifo(path, 0o600))
	candidate := newApplyPatchToolWithAccess(dir, nil, directFileMutator{}, nil)
	params := marshalApplyPatchParams(t, "--- a/target\n+++ b/target\n@@ -1 +1 @@\n-before\n+after\n")
	done := make(chan error, 1)
	go func() {
		_, err := candidate.HookCalls(t.Context(), params)
		done <- err
	}()

	select {
	case err := <-done:
		require.EqualError(t, err, "not a regular file: "+path)
	case <-time.After(2 * time.Second):
		t.Fatal("FIFO projection did not return within two seconds")
	}
}

func TestApplyPatchHookedDispatchRejectsFIFO(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	require.NoError(t, syscall.Mkfifo(path, 0o600))
	candidate := newApplyPatchToolWithAccess(dir, nil, directFileMutator{}, nil)
	params := marshalApplyPatchParams(t, "--- a/target\n+++ b/target\n@@ -1 +1 @@\n-before\n+after\n")
	_, executeErr := candidate.Execute(t.Context(), params)
	require.Error(t, executeErr)
	ctx := tool.WithCallHooks(t.Context(), &secondEditRefusingHooks{})
	done := make(chan tool.CallResult, 1)
	go func() {
		report := tool.Schedule(ctx, []tool.Call{{
			Tool:      candidate,
			Name:      "apply_patch",
			Arguments: params,
		}})
		done <- report.Results[0]
	}()

	select {
	case result := <-done:
		assert.Equal(t, tool.OutcomeFailed, result.Outcome)
		require.EqualError(t, result.Err, "execute tool apply_patch: "+executeErr.Error())
	case <-time.After(2 * time.Second):
		t.Fatal("hooked FIFO dispatch did not return within two seconds")
	}
}
