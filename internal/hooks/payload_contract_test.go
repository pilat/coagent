package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/tool"
)

type nilHookResponder struct {
	*hookTool
}

func (*nilHookResponder) HookResponse(*tool.Result) any { return nil }

func TestHookStdinPayloadContract(t *testing.T) {
	t.Run("PreToolUse", pinPreToolPayload)
	t.Run("PostToolUse", pinPostToolPayload)
	t.Run("Stop", pinStopPayload)
	t.Run("PostToolUse null response", pinNullPostResponse)
}

func pinPreToolPayload(t *testing.T) {
	f, hooks, captured := contractHooks(t, "PreToolUse")
	ctx := tool.WithCallID(f.ctx, "call-123")
	call := hookCall("read", "Read", `{"file_path":"file.txt","number":9007199254740993,"nullable":null}`)
	assert.Empty(t, hooks.BeforeCall(ctx, call).Refusal)
	assert.Equal(t, map[string]any{
		"session_id":        "42",
		"cwd":               f.dir,
		"hook_event_name":   "PreToolUse",
		"tool_name":         "Read",
		"coagent_tool_name": "read",
		"tool_use_id":       "call-123",
		"tool_input": map[string]any{
			"file_path": filepath.Join(f.dir, "file.txt"),
			"number":    json.Number("9007199254740993"),
			"nullable":  nil,
		},
	}, decodedContract(t, *captured))
}

func pinPostToolPayload(t *testing.T) {
	f, hooks, captured := contractHooks(t, "PostToolUse")
	ctx := tool.WithCallID(f.ctx, "call-123")
	call := hookCall("read", "Read", `[9007199254740993,null,"unchanged"]`)
	assert.Empty(t, hooks.AfterCall(ctx, call, &tool.Result{Output: "quoted \"output\"\n"}))
	assert.Equal(t, map[string]any{
		"session_id":        "42",
		"cwd":               f.dir,
		"hook_event_name":   "PostToolUse",
		"tool_name":         "Read",
		"coagent_tool_name": "read",
		"tool_use_id":       "call-123",
		"tool_input":        []any{json.Number("9007199254740993"), nil, "unchanged"},
		"tool_response":     map[string]any{"output": "quoted \"output\"\n"},
	}, decodedContract(t, *captured))
}

func pinStopPayload(t *testing.T) {
	f, hooks, captured := contractHooks(t, "Stop")
	reason, err := hooks.Stop(f.ctx, "the final answer", false)
	require.NoError(t, err)
	assert.Empty(t, reason)
	assert.Equal(t, map[string]any{
		"session_id":             "42",
		"cwd":                    f.dir,
		"hook_event_name":        "Stop",
		"stop_hook_active":       false,
		"last_assistant_message": "the final answer",
	}, decodedContract(t, *captured))
}

func pinNullPostResponse(t *testing.T) {
	f, hooks, captured := contractHooks(t, "PostToolUse")
	call := hookCall("native", "", `{}`)
	call.Tool = &nilHookResponder{hookTool: &hookTool{id: "native"}}
	assert.Empty(t, hooks.AfterCall(f.ctx, call, &tool.Result{Output: "original"}))
	payload := decodedContract(t, *captured)
	assert.Contains(t, payload, "tool_response")
	assert.Nil(t, payload["tool_response"])
}

func contractHooks(t *testing.T, event string) (hookFixture, *Hooks, *[]byte) {
	t.Helper()
	f := newHookFixture(t)
	f.settings(t, "settings.json", hookSettings(event, "*", commandHandler("guard")))
	var captured []byte
	launch := func(_ context.Context, process Process) (Outcome, error) {
		captured = append(captured, process.Stdin...)
		return Outcome{}, nil
	}
	hooks, err := Load(f.ctx, f.dir, 42, launch)
	require.NoError(t, err)
	require.NotNil(t, hooks)
	return f, hooks, &captured
}

func decodedContract(t *testing.T, data []byte) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var payload map[string]any
	require.NoError(t, decoder.Decode(&payload))
	return payload
}
