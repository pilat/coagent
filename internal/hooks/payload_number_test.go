package hooks

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPreToolUsePreservesExactJSONNumbers(t *testing.T) {
	f := newHookFixture(t)
	f.settings(t, "settings.json", hookSettings("PreToolUse", "*", commandHandler("guard")))
	var payload map[string]json.RawMessage
	launch := func(_ context.Context, process Process) (Outcome, error) {
		require.NoError(t, json.Unmarshal(process.Stdin, &payload))
		return Outcome{}, nil
	}
	hooks, err := Load(f.ctx, f.dir, 42, launch)
	require.NoError(t, err)
	for _, input := range []string{`{"number":9007199254740993}`, `{"file_path":"a","number":9007199254740993}`} {
		assert.Empty(t, hooks.BeforeCall(f.ctx, hookCall("native", "", input)).Refusal)
		var fields map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(payload["tool_input"], &fields))
		assert.Equal(t, "9007199254740993", string(fields["number"]))
	}
}

func TestPreToolUseLeavesNullFilePathUnchanged(t *testing.T) {
	f := newHookFixture(t)
	f.settings(t, "settings.json", hookSettings("PreToolUse", "*", commandHandler("guard")))
	var payload map[string]json.RawMessage
	launch := func(_ context.Context, process Process) (Outcome, error) {
		require.NoError(t, json.Unmarshal(process.Stdin, &payload))
		return Outcome{}, nil
	}
	hooks, err := Load(f.ctx, f.dir, 42, launch)
	require.NoError(t, err)
	assert.Empty(t, hooks.BeforeCall(f.ctx, hookCall("native", "", `{"file_path":null}`)).Refusal)
	assert.JSONEq(t, `{"file_path":null}`, string(payload["tool_input"]))
}
