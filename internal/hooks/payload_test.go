package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/tool"
)

func TestPreToolUsePayloadFields(t *testing.T) {
	f := newHookFixture(t)
	path := filepath.Join(f.dir, "payload.json")
	envPath := filepath.Join(f.dir, "env")
	command := "cat > " + shellQuote(path) + "; printf '%s' \"$CLAUDE_PROJECT_DIR\" > " + shellQuote(envPath)
	hooks := f.command(t, commandHandler(command))
	ctx := tool.WithCallID(f.ctx, "call-123")
	assert.Empty(t, hooks.BeforeCall(ctx, bashCall()).Refusal)
	payload := readHookPayload(t, path)
	assert.Equal(t, "42", payload["session_id"])
	assert.Equal(t, f.dir, payload["cwd"])
	assert.Equal(t, "PreToolUse", payload["hook_event_name"])
	assert.Equal(t, "Bash", payload["tool_name"])
	assert.Equal(t, "bash", payload["coagent_tool_name"])
	assert.Equal(t, "call-123", payload["tool_use_id"])
	assert.Equal(t, map[string]any{"command": "echo hello"}, payload["tool_input"])
	for _, key := range []string{"transcript_path", "permission_mode", "prompt_id", "effort", "agent_id", "agent_type"} {
		assert.NotContains(t, payload, key)
	}
	env, err := os.ReadFile(envPath)
	require.NoError(t, err)
	assert.Equal(t, f.dir, string(env))
	assert.Empty(t, hooks.BeforeCall(ctx, hookCall("read", "Read", `{"file_path":"file.txt"}`)).Refusal)
	readPayload := readHookPayload(t, path)
	assert.Equal(t, filepath.Join(f.dir, "file.txt"), readPayload["tool_input"].(map[string]any)["file_path"])
}

func TestPreToolUseMCPToolKeepsNativeName(t *testing.T) {
	f := newHookFixture(t)
	path := filepath.Join(f.dir, "payload.json")
	hooks := f.command(t, commandHandler("cat > "+shellQuote(path)))
	name := "mcp__memory__read"
	assert.Empty(t, hooks.BeforeCall(f.ctx, hookCall(name, "", `{}`)).Refusal)
	payload := readHookPayload(t, path)
	assert.Equal(t, name, payload["tool_name"])
	assert.Equal(t, name, payload["coagent_tool_name"])
}

func TestPreToolUseFilePathNormalization(t *testing.T) {
	f := newHookFixture(t)
	path := filepath.Join(f.dir, "payload.json")
	hooks := f.command(t, commandHandler("cat > "+shellQuote(path)))
	tests := []struct {
		input string
		want  string
	}{
		{input: "~/x", want: filepath.Join(f.home, "x")},
		{input: "~", want: f.home},
		{input: "/tmp/dir/../x", want: "/tmp/x"},
		{input: "relative/x", want: filepath.Join(f.dir, "relative/x")},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			input, err := json.Marshal(map[string]string{"file_path": tt.input})
			require.NoError(t, err)
			assert.Empty(t, hooks.BeforeCall(f.ctx, hookCall("read", "Read", string(input))).Refusal)
			payload := readHookPayload(t, path)
			assert.Equal(t, tt.want, payload["tool_input"].(map[string]any)["file_path"])
		})
	}
}
