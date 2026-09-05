package builtin

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
)

// Conditional registration: the model never sees a search tool that always
// errors. The tool is absent from the registry and from the wire schemas
// unless a provider is configured.
func TestBuildStack_ConditionalSearchRegistration(t *testing.T) {
	t.Parallel()

	tavilyEnabled := func() *config.UnifiedConfig {
		unified := &config.UnifiedConfig{}
		unified.Tools.Search = config.SearchToolConfig{
			Provider:   config.SearchProviderTavily,
			APIKey:     "tvly-test",
			MaxResults: 7,
		}

		return unified
	}()

	disabled := &config.UnifiedConfig{}
	disabled.Tools.Search.Provider = config.SearchProviderTavily
	off := false
	disabled.Tools.Search.Enabled = &off

	tests := []struct {
		name     string
		unified  *config.UnifiedConfig
		register bool
	}{
		{name: "tavily configured registers websearch", unified: tavilyEnabled, register: true},
		{name: "no tools.search omits websearch", unified: &config.UnifiedConfig{}, register: false},
		{name: "nil unified omits websearch", unified: nil, register: false},
		{name: "enabled false omits websearch", unified: disabled, register: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stack, err := BuildStack(context.Background(), StackConfig{
				WorkDir: t.TempDir(),
				Unified: tt.unified,
				Loader:  loader.New(),
				Todo:    todo.New(),
			})
			require.NoError(t, err)

			t.Cleanup(func() { _ = stack.Close() })

			ids := stack.Registry.IDs()
			if !tt.register {
				assert.NotContains(t, ids, websearchToolID)

				return
			}

			assert.Contains(t, ids, websearchToolID)

			// The model surface (wire schemas) carries the tool too.
			schemas := tool.ToSchemas(stack.Registry.List())
			var schemaIDs []string
			for _, s := range schemas {
				schemaIDs = append(schemaIDs, s.Name)
			}

			assert.Contains(t, schemaIDs, websearchToolID)

			tl := stack.Registry.Get(websearchToolID)
			assert.Equal(t, 7, tl.(*webSearchTool).maxResults, "config max_results feeds the tool")
		})
	}
}
