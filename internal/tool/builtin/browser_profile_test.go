package builtin

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/mcp"
	"github.com/pilat/coagent/internal/tool"
)

func TestBrowserProfileGate(t *testing.T) {
	for _, raw := range []string{`{}`, `{"task_state":"  "}`, `{"task_state":42}`, `broken`} {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			var args map[string]any
			decodeErr := json.Unmarshal([]byte(raw), &args)
			got := (browserProfile{}).Gate(args, decodeErr)
			assert.Equal(t, &tool.Result{
				IsError: true,
				Output:  "Every playwright tool call requires a non-blank string task_state recording the previous action's outcome, task facts from the current frame, and the next goal.",
			}, got)
		})
	}

	t.Run("valid state is stripped", func(t *testing.T) {
		t.Parallel()
		args := map[string]any{"task_state": " continue ", "x": float64(1)}
		assert.Nil(t, (browserProfile{}).Gate(args, nil))
		assert.Equal(t, map[string]any{"x": float64(1)}, args)
	})
}

func TestBrowserProfileSchema(t *testing.T) {
	for _, tc := range []struct {
		name     string
		input    string
		required []any
	}{
		{"existing property", `{"type":"object","properties":{"x":{"type":"integer"}},"required":["x"]}`, []any{"x", "task_state"}},
		{"parameterless", `{"type":"object"}`, []any{"task_state"}},
		{"invalid", `broken`, []any{"task_state"}},
		{"null", `null`, []any{"task_state"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			input := json.RawMessage(tc.input)
			var got map[string]any
			require.NoError(t, json.Unmarshal((browserProfile{}).Schema(input), &got))
			assert.Equal(t, tc.input, string(input))
			assert.Equal(t, "object", got["type"])
			assert.Equal(t, tc.required, got["required"])
			properties, ok := got["properties"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, map[string]any{
				"type":        "string",
				"description": "In up to five sentences, say whether the previous action succeeded, failed, or is uncertain based on the current frame; preserve task facts from this frame (names, prices, URLs, counts, exclusions); and name the next immediate goal. Do not describe screen coordinates or layout.",
			}, properties["task_state"])
			if tc.name == "existing property" {
				assert.Equal(t, map[string]any{"type": "integer"}, properties["x"])
			} else {
				assert.Len(t, properties, 1)
			}
		})
	}
}

func TestBrowserProfileAdmit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		schema string
		admit  bool
	}{
		{"collision", `{"type":"object","properties":{"task_state":{"type":"string"}}}`, false},
		{"null collision", `{"type":"object","properties":{"task_state":null}}`, false},
		{"other properties", `{"type":"object","properties":{"state":{"type":"string"}}}`, true},
		{"no properties", `{"type":"object"}`, true},
		{"invalid", `broken`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.admit, (browserProfile{}).Admit(mcp.ToolMeta{
				Name: "click", Schema: json.RawMessage(tc.schema),
			}))
		})
	}
}
