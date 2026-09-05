package ctl

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pilat/coagent/internal/config"
)

func unifiedForSearchStatus(models map[string]string) *config.UnifiedConfig {
	uc := &config.UnifiedConfig{
		Providers: map[string]config.ProviderEntry{
			"openrouter": {Driver: "openrouter", APIKey: "k", BaseURL: "https://openrouter.ai/api/v1"},
			"anthropic":  {Driver: "anthropic", APIKey: "k"},
		},
	}

	for id, provider := range models {
		uc.Models = append(uc.Models, config.ModelEntry{ID: id, Provider: provider})
	}

	return uc
}

func TestSearchStatus(t *testing.T) {
	t.Parallel()

	off := false

	tavily := unifiedForSearchStatus(map[string]string{"ant": "anthropic"})
	tavily.Tools.Search = config.SearchToolConfig{Provider: config.SearchProviderTavily, APIKey: "tvly-test"}

	searxng := unifiedForSearchStatus(map[string]string{"ant": "anthropic"})
	searxng.Tools.Search = config.SearchToolConfig{
		Provider: config.SearchProviderSearxng,
		BaseURL:  "https://searx.example.com",
	}

	disabled := unifiedForSearchStatus(map[string]string{"or": "openrouter"})
	disabled.Tools.Search.Enabled = &off

	tests := []struct {
		name         string
		unified      *config.UnifiedConfig
		defaultModel string
		want         string
	}{
		{
			name:         "native passthrough on the default model",
			unified:      unifiedForSearchStatus(map[string]string{"or": "openrouter"}),
			defaultModel: "or",
			want:         "native (openrouter)",
		},
		{
			name:         "unconfigured with a non-native default renders empty",
			unified:      unifiedForSearchStatus(map[string]string{"ant": "anthropic"}),
			defaultModel: "ant",
			want:         "",
		},
		{
			name:         "tavily",
			unified:      tavily,
			defaultModel: "ant",
			want:         "tavily",
		},
		{
			name:         "searxng shows the instance",
			unified:      searxng,
			defaultModel: "ant",
			want:         "searxng (https://searx.example.com)",
		},
		{
			name:         "disabled",
			unified:      disabled,
			defaultModel: "or",
			want:         "disabled",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, searchStatus(tt.unified, tt.defaultModel))
		})
	}
}
