package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/tool/builtin"
)

func TestPostToolUseExitTwoFeedbackReachesResult(t *testing.T) {
	f := newHookFixture(t)
	command := "printf 'check the output' >&2; exit 2"
	f.settings(t, "settings.json", hookSettings("PostToolUse", "*", commandHandler(command)))
	hooks := f.load(t)
	require.NotNil(t, hooks)
	report := tool.Schedule(tool.WithCallHooks(f.ctx, hooks), []tool.Call{bashCall()})
	require.Len(t, report.Results, 1)
	result := report.Results[0]
	require.Equal(t, tool.OutcomeExecuted, result.Outcome)
	require.NotNil(t, result.Result)
	assert.Equal(t, "executed", result.Result.Output)
	assert.False(t, result.Result.IsError)
	assert.Equal(
		t,
		[]string{"PostToolUse:Bash hook feedback: [" + command + "]: check the output"},
		result.Result.HookFeedback,
	)
}

func TestPostToolUseJSONBlockReasonAndAdditionalContext(t *testing.T) {
	f := newHookFixture(t)
	f.settings(t, "settings.json", hookSettings("PostToolUse", "*", commandHandler("guard")))
	recorder := &hookPayloadRecorder{respond: func(map[string]any) Outcome {
		return Outcome{
			Stdout: []byte(
				`{"decision":"block","reason":"review output","hookSpecificOutput":{"additionalContext":"inspect details"}}`,
			),
		}
	}}
	hooks, err := Load(f.ctx, f.dir, 42, recorder.launch)
	require.NoError(t, err)
	require.NotNil(t, hooks)
	result := &tool.Result{Output: "unchanged"}
	feedback := hooks.AfterCall(f.ctx, bashCall(), result)
	assert.Contains(t, strings.Join(feedback, "\n"), "review output")
	assert.Contains(t, strings.Join(feedback, "\n"), "inspect details")
	assert.Equal(t, "unchanged", result.Output)
}

func TestPostToolUseBashResponseShape(t *testing.T) {
	f := newHookFixture(t)
	f.settings(t, "settings.json", hookSettings("PostToolUse", "Bash", commandHandler("guard")))
	stack, err := builtin.BuildStack(f.ctx, builtin.StackConfig{WorkDir: f.dir})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, stack.Close()) })
	recorder := &hookPayloadRecorder{respond: func(payload map[string]any) Outcome {
		response, _ := payload["tool_response"].(map[string]any)
		encoded, _ := json.Marshal(map[string]any{"decision": "block", "reason": response["stdout"]})
		return Outcome{Stdout: encoded}
	}}
	hooks, err := Load(f.ctx, f.dir, 42, recorder.launch)
	require.NoError(t, err)
	require.NotNil(t, hooks)
	call := tool.Call{
		Name:      "bash",
		Tool:      stack.Registry.Get("bash"),
		Arguments: json.RawMessage(`{"command":"echo output"}`),
	}
	feedback := hooks.AfterCall(f.ctx, call, &tool.Result{Output: "command output"})
	require.Len(t, recorder.payloads, 1)
	assert.Equal(t, map[string]any{
		"stdout": "command output", "stderr": "", "interrupted": false, "isImage": false,
	}, recorder.payloads[0]["tool_response"])
	assert.Contains(t, strings.Join(feedback, "\n"), "command output")
}

func TestPostToolUseDefaultResponseIsOutputObject(t *testing.T) {
	f := newHookFixture(t)
	f.settings(t, "settings.json", hookSettings("PostToolUse", "*", commandHandler("guard")))
	recorder := &hookPayloadRecorder{}
	hooks, err := Load(f.ctx, f.dir, 42, recorder.launch)
	require.NoError(t, err)
	require.NotNil(t, hooks)
	feedback := hooks.AfterCall(f.ctx, hookCall("native", "", `{}`), &tool.Result{Output: "native output"})
	assert.Empty(t, feedback)
	require.Len(t, recorder.payloads, 1)
	assert.Equal(t, map[string]any{"output": "native output"}, recorder.payloads[0]["tool_response"])
	assert.Equal(t, "PostToolUse", recorder.payloads[0]["hook_event_name"])
}

func TestPostToolUseRunsPerProjectedHunk(t *testing.T) {
	f := newHookFixture(t)
	f.settings(t, "settings.json", hookSettings("PostToolUse", "Edit", commandHandler("guard")))
	recorder := &hookPayloadRecorder{}
	hooks, err := Load(f.ctx, f.dir, 42, recorder.launch)
	require.NoError(t, err)
	require.NotNil(t, hooks)
	call := projectedPatchCall([]tool.HookCall{
		{
			ToolID: "edit",
			Name:   tool.HookName{Name: "Edit"},
			Input:  json.RawMessage(`{"file_path":"a.txt","old_string":"first","new_string":"one"}`),
		},
		{
			ToolID: "edit",
			Name:   tool.HookName{Name: "Edit"},
			Input:  json.RawMessage(`{"file_path":"a.txt","old_string":"second","new_string":"two"}`),
		},
	})
	assert.Empty(t, hooks.AfterCall(f.ctx, call, &tool.Result{Output: "patched"}))
	require.Len(t, recorder.payloads, 2)
	for i, old := range []string{"first", "second"} {
		payload := recorder.payloads[i]
		assert.Equal(t, "Edit", payload["tool_name"])
		assert.Equal(t, "apply_patch", payload["coagent_tool_name"])
		assert.Equal(t, old, payload["tool_input"].(map[string]any)["old_string"])
	}
}

func TestPostToolUseNewFileKeepsWriteProjection(t *testing.T) {
	f := newHookFixture(t)
	f.settings(t, "settings.json", hookSettings("PostToolUse", "Write", commandHandler("guard")))
	stack, err := builtin.BuildStack(f.ctx, builtin.StackConfig{WorkDir: f.dir})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, stack.Close()) })
	recorder := &hookPayloadRecorder{}
	hooks, err := Load(f.ctx, f.dir, 42, recorder.launch)
	require.NoError(t, err)
	require.NotNil(t, hooks)
	params, err := json.Marshal(map[string]string{"patch": "--- /dev/null\n+++ b/new.txt\n@@ -0,0 +1,1 @@\n+created"})
	require.NoError(t, err)
	call := tool.Call{Name: "apply_patch", Tool: stack.Registry.Get("apply_patch"), Arguments: params}
	report := tool.Schedule(tool.WithCallHooks(f.ctx, hooks), []tool.Call{call})
	require.Len(t, report.Results, 1)
	require.NoError(t, report.Results[0].Err)
	require.Equal(t, tool.OutcomeExecuted, report.Results[0].Outcome)
	content, err := os.ReadFile(filepath.Join(f.dir, "new.txt"))
	require.NoError(t, err)
	assert.Equal(t, "created", string(content))
	require.Len(t, recorder.payloads, 1)
	payload := recorder.payloads[0]
	assert.Equal(t, "Write", payload["tool_name"])
	assert.Equal(t, "apply_patch", payload["coagent_tool_name"])
	assert.Equal(
		t,
		map[string]any{"file_path": filepath.Join(f.dir, "new.txt"), "content": "created"},
		payload["tool_input"],
	)
}
