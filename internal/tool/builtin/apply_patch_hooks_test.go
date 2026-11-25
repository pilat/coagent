package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
)

type secondEditRefusingHooks struct {
	edits int
}

func (h *secondEditRefusingHooks) BeforeCall(_ context.Context, call tool.Call) tool.HookVerdict {
	for _, projected := range call.Projection {
		if projected.ToolID == "edit" {
			h.edits++
			if h.edits == 2 {
				return tool.HookVerdict{Refusal: "second Edit is forbidden"}
			}
		}
	}

	return tool.HookVerdict{}
}

func (h *secondEditRefusingHooks) AfterCall(context.Context, tool.Call, *tool.Result) []string {
	return nil
}

func TestApplyPatchHookCallsOneEditPerHunk(t *testing.T) {
	workDir := t.TempDir()
	path := filepath.Join(workDir, "file.txt")
	require.NoError(t, os.WriteFile(path, []byte("one\nbefore\nmiddle\nthree\nlast\n"), 0o644))
	candidate := newApplyPatchToolWithAccess(workDir, nil, directFileMutator{}, nil)
	assert.Equal(t, tool.HookName{Aliases: []string{"patch"}}, candidate.HookName())
	params := marshalApplyPatchParams(t, `--- a/file.txt
+++ b/file.txt
@@ -1,2 +1,2 @@
 one
-before
+changed
@@ -4,2 +4,3 @@
 three
-last
+after
+appended`)
	calls, err := candidate.HookCalls(context.Background(), params)
	require.NoError(t, err)
	require.Len(t, calls, 2)
	wants := []editParams{
		{FilePath: path, OldString: "one\nbefore", NewString: "one\nchanged", ReplaceAll: false},
		{FilePath: path, OldString: "three\nlast", NewString: "three\nafter\nappended", ReplaceAll: false},
	}
	for i, call := range calls {
		assert.Equal(t, "edit", call.ToolID)
		assert.Equal(t, editHookName, call.Name)
		var got editParams
		require.NoError(t, json.Unmarshal(call.Input, &got))
		assert.Equal(t, wants[i], got)
		assert.Contains(t, string(call.Input), `"replace_all":false`)
	}
}

func TestApplyPatchHookCallsWriteForNewFile(t *testing.T) {
	workDir := t.TempDir()
	path := filepath.Join(workDir, "nested", "new.txt")
	candidate := newApplyPatchToolWithAccess(workDir, nil, directFileMutator{}, nil)
	params := marshalApplyPatchParams(t, `--- /dev/null
+++ b/nested/new.txt
@@ -0,0 +1,2 @@
+created
+next`)
	calls, err := candidate.HookCalls(context.Background(), params)
	require.NoError(t, err)
	require.Len(t, calls, 1)
	assert.Equal(t, "write", calls[0].ToolID)
	assert.Equal(t, writeHookName, calls[0].Name)
	var projected writeParams
	require.NoError(t, json.Unmarshal(calls[0].Input, &projected))
	assert.Equal(t, path, projected.FilePath)
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = candidate.Execute(context.Background(), params)
	require.NoError(t, err)
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(content), projected.Content)
}

func TestApplyPatchHookCallsParseErrorReturnsError(t *testing.T) {
	candidate := newApplyPatchToolWithAccess(t.TempDir(), nil, directFileMutator{}, nil)
	tests := []struct {
		name   string
		params json.RawMessage
	}{
		{name: "invalid JSON", params: json.RawMessage(`{`)},
		{name: "missing patch", params: json.RawMessage(`{}`)},
		{name: "empty patch", params: marshalApplyPatchParams(t, "")},
		{name: "malformed patch", params: marshalApplyPatchParams(t, "not a unified patch")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls, err := candidate.HookCalls(context.Background(), tt.params)
			require.Error(t, err)
			assert.Nil(t, calls)
		})
	}
}

func TestApplyPatchRefusedByEditGuardLeavesFilesUntouched(t *testing.T) {
	workDir := t.TempDir()
	first := filepath.Join(workDir, "first.txt")
	second := filepath.Join(workDir, "second.txt")
	require.NoError(t, os.WriteFile(first, []byte("first before"), 0o644))
	require.NoError(t, os.WriteFile(second, []byte("second before"), 0o644))
	stack, err := BuildStack(context.Background(), StackConfig{
		WorkDir: workDir, Loader: loader.New(), Todo: todo.New(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = stack.Close() })
	params := marshalApplyPatchParams(t, `--- a/first.txt
+++ b/first.txt
@@ -1,1 +1,1 @@
-first before
+first after
--- a/second.txt
+++ b/second.txt
@@ -1,1 +1,1 @@
-second before
+second after`)
	hooks := &secondEditRefusingHooks{}
	ctx := tool.WithCallHooks(context.Background(), hooks)
	report := tool.Schedule(ctx, []tool.Call{{
		Tool: stack.Registry.Get("apply_patch"), Name: "apply_patch", Arguments: params,
	}})
	require.Len(t, report.Results, 1)
	assert.Equal(t, tool.OutcomeFailed, report.Results[0].Outcome)
	require.NotNil(t, report.Results[0].Result)
	assert.Equal(t, "second Edit is forbidden", report.Results[0].Result.Output)
	assert.Equal(t, 2, hooks.edits)
	firstContent, err := os.ReadFile(first)
	require.NoError(t, err)
	secondContent, err := os.ReadFile(second)
	require.NoError(t, err)
	assert.Equal(t, "first before", string(firstContent))
	assert.Equal(t, "second before", string(secondContent))
}
