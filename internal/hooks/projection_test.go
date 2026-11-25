package hooks

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/tool"
)

type hookPayloadRecorder struct {
	mu       sync.Mutex
	payloads []map[string]any
	respond  func(map[string]any) Outcome
}

func (r *hookPayloadRecorder) launch(_ context.Context, process Process) (Outcome, error) {
	var payload map[string]any
	if err := json.Unmarshal(process.Stdin, &payload); err != nil {
		return Outcome{}, err
	}
	r.mu.Lock()
	r.payloads = append(r.payloads, payload)
	r.mu.Unlock()
	if r.respond != nil {
		return r.respond(payload), nil
	}
	return Outcome{}, nil
}

func TestApplyPatchMatchesEditWriteAndNativeNames(t *testing.T) {
	tests := []struct {
		matcher string
		names   []string
	}{
		{matcher: "Edit", names: []string{"Edit"}},
		{matcher: "Write", names: []string{"Write"}},
		{matcher: "apply_patch", names: []string{"Edit", "Write"}},
		{matcher: "patch", names: []string{"Edit", "Write"}},
	}
	for _, tt := range tests {
		t.Run(tt.matcher, func(t *testing.T) {
			f := newHookFixture(t)
			f.settings(t, "settings.json", hookSettings("PreToolUse", tt.matcher, commandHandler("guard")))
			recorder := &hookPayloadRecorder{}
			hooks, err := Load(f.ctx, f.dir, 42, recorder.launch)
			require.NoError(t, err)
			require.NotNil(t, hooks)
			call := projectedPatchCall([]tool.HookCall{
				{
					ToolID: "edit",
					Name:   tool.HookName{Name: "Edit"},
					Input: json.RawMessage(
						`{"file_path":"a.txt","old_string":"old","new_string":"new","replace_all":false}`,
					),
				},
				{
					ToolID: "write",
					Name:   tool.HookName{Name: "Write"},
					Input:  json.RawMessage(`{"file_path":"b.txt","content":"new file"}`),
				},
			})
			assert.Empty(t, hooks.BeforeCall(f.ctx, call).Refusal)
			require.Len(t, recorder.payloads, len(tt.names))
			for i, payload := range recorder.payloads {
				assert.Equal(t, tt.names[i], payload["tool_name"])
				assert.Equal(t, "apply_patch", payload["coagent_tool_name"])
				assert.Contains(t, payload["tool_input"].(map[string]any)["file_path"], f.dir)
			}
		})
	}
}

func TestApplyPatchOneRefusedHunkRefusesWholePatch(t *testing.T) {
	f := newHookFixture(t)
	f.settings(t, "settings.json", hookSettings("PreToolUse", "Edit", commandHandler("guard")))
	recorder := &hookPayloadRecorder{respond: func(payload map[string]any) Outcome {
		input := payload["tool_input"].(map[string]any)
		if input["old_string"] == "second" {
			return Outcome{ExitCode: 2, Stderr: []byte("second hunk blocked")}
		}
		return Outcome{}
	}}
	hooks, err := Load(f.ctx, f.dir, 42, recorder.launch)
	require.NoError(t, err)
	require.NotNil(t, hooks)
	call := projectedPatchCall([]tool.HookCall{
		{
			ToolID: "edit",
			Name:   tool.HookName{Name: "Edit"},
			Input: json.RawMessage(
				`{"file_path":"a.txt","old_string":"first","new_string":"one","replace_all":false}`,
			),
		},
		{
			ToolID: "edit",
			Name:   tool.HookName{Name: "Edit"},
			Input: json.RawMessage(
				`{"file_path":"a.txt","old_string":"second","new_string":"two","replace_all":false}`,
			),
		},
	})
	verdict := hooks.BeforeCall(f.ctx, call)
	assert.Equal(t, "PreToolUse:Edit hook error: [guard]: second hunk blocked", verdict.Refusal)
	require.Len(t, recorder.payloads, 2)
	assert.Equal(t, "first", recorder.payloads[0]["tool_input"].(map[string]any)["old_string"])
	assert.Equal(t, "second", recorder.payloads[1]["tool_input"].(map[string]any)["old_string"])
}

func projectedPatchCall(calls []tool.HookCall) tool.Call {
	return tool.Call{
		Name: "apply_patch",
		Tool: &hookTool{
			id:   "apply_patch",
			name: tool.HookName{Aliases: []string{"patch"}},
		},
		Arguments:  json.RawMessage(`{"patch":"original patch"}`),
		Projection: calls,
	}
}
