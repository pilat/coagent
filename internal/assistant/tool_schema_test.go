package assistant

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAssistantForwardCannotParaphrase(t *testing.T) {
	for _, entry := range toolTable {
		if entry.schema.Name != "forward" {
			continue
		}
		var params struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		}
		require.NoError(t, json.Unmarshal(entry.schema.Parameters, &params))
		assert.Equal(t, []string{"session"}, slices.Sorted(maps.Keys(params.Properties)))
		assert.Equal(t, []string{"session"}, params.Required)
		return
	}
	t.Fatal("forward tool is missing")
}

func TestAssistantSessionSchemasUseStrings(t *testing.T) {
	t.Parallel()
	for _, entry := range toolTable {
		spec := entry.schema
		t.Run(spec.Name, func(t *testing.T) {
			var params struct {
				Properties map[string]struct {
					Type        string `json:"type"`
					Description string `json:"description"`
				} `json:"properties"`
			}
			require.NoError(t, json.Unmarshal(spec.Parameters, &params))
			if spec.Name == "open" {
				assert.Equal(t, "string", params.Properties["model"].Type)
				assert.Equal(t, "string", params.Properties["effort"].Type)
				assert.Equal(t, "string", params.Properties["worktree"].Type)
				assert.Equal(
					t,
					"One line, why this session exists (1-80 characters).",
					params.Properties["about"].Description,
				)
			}
			if session, ok := params.Properties["session"]; ok {
				assert.Equal(t, "string", session.Type)
				assert.Contains(t, session.Description, `"#244"`)
			}
		})
	}
}
