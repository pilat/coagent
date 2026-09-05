package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pilat/coagent/internal/config"
)

func unifiedForNotice(models map[string]string) *config.UnifiedConfig {
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

func TestSearchUnconfigured(t *testing.T) {
	t.Parallel()

	off := false

	disabled := unifiedForNotice(map[string]string{"m1": "anthropic"})
	disabled.Tools.Search.Enabled = &off

	provider := unifiedForNotice(map[string]string{"m1": "anthropic"})
	provider.Tools.Search = config.SearchToolConfig{
		Provider: config.SearchProviderTavily,
		APIKey:   "tvly-test",
	}

	tests := []struct {
		name    string
		unified *config.UnifiedConfig
		want    bool
	}{
		{
			name:    "nil unified config stays silent",
			unified: nil,
			want:    false,
		},
		{
			name:    "no search config and only non-native models fires",
			unified: unifiedForNotice(map[string]string{"m1": "anthropic"}),
			want:    true,
		},
		{
			name:    "one native-capable model suppresses the notice",
			unified: unifiedForNotice(map[string]string{"or1": "openrouter", "m1": "anthropic"}),
			want:    false,
		},
		{
			name:    "enabled false is a choice, not an omission",
			unified: disabled,
			want:    false,
		},
		{
			name:    "any explicit provider stays silent",
			unified: provider,
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, searchUnconfigured(tt.unified))
		})
	}
}
