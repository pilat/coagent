package builtin

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/tool"
)

func TestBashHookResponseJSONShape(t *testing.T) {
	tests := []struct {
		name   string
		output string
	}{
		{
			name:   "plain text",
			output: "command output\nincluding stderr\n",
		},
		{
			name:   "JSON-looking text",
			output: `{"message":"still plain stdout"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := &bashTool{}
			encoded, err := json.Marshal(candidate.HookResponse(&tool.Result{
				Output: tt.output,
			}))
			require.NoError(t, err)
			var decoded map[string]any
			require.NoError(t, json.Unmarshal(encoded, &decoded))
			assert.Equal(t, map[string]any{
				"stdout":      tt.output,
				"stderr":      "",
				"interrupted": false,
				"isImage":     false,
			}, decoded)
		})
	}
}
