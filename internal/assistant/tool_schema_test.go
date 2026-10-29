package assistant

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAssistantSessionSchemasUseStrings(t *testing.T) {
	t.Parallel()
	for _, spec := range toolSchemas {
		t.Run(spec.Name, func(t *testing.T) {
			var params struct {
				Properties map[string]struct {
					Type        string `json:"type"`
					Description string `json:"description"`
				} `json:"properties"`
			}
			require.NoError(t, json.Unmarshal(spec.Parameters, &params))
			if session, ok := params.Properties["session"]; ok {
				assert.Equal(t, "string", session.Type)
				assert.Contains(t, session.Description, `"#244"`)
			}
		})
	}
}
