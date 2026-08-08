package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pilat/coagent/internal/config"
)

// A provider arrives with the models that make it usable, or the first run ends
// on a config no session can start on.
func TestProviderModels(t *testing.T) {
	tests := []struct {
		name     string
		entry    config.ProviderEntry
		explicit []string
		want     []string
	}{
		{
			name:  "anthropic gets its recommendation",
			entry: config.ProviderEntry{Driver: "anthropic"},
			want:  []string{"claude-sonnet-5"},
		},
		{
			name:  "openrouter gets its recommendation",
			entry: config.ProviderEntry{Driver: "openrouter"},
			want:  []string{"anthropic/claude-sonnet-5"},
		},
		{
			name:  "google-sa resolves through the vertex catalog",
			entry: config.ProviderEntry{Driver: "google-sa"},
			want:  []string{"gemini-3.5-flash"},
		},
		{
			name:  "a bare openai endpoint gets nothing — the bootstrap asks instead",
			entry: config.ProviderEntry{Driver: "openai"},
			want:  nil,
		},
		{
			name:  "an openai endpoint that names its catalog does get one",
			entry: config.ProviderEntry{Driver: "openai", Catalog: "openai"},
			want:  []string{"gpt-5-mini"},
		},
		{
			name:     "an explicit list always wins",
			entry:    config.ProviderEntry{Driver: "anthropic"},
			explicit: []string{"claude-opus-5"},
			want:     []string{"claude-opus-5"},
		},
		{
			name:     "an explicit list for a vendor with no recommendation",
			entry:    config.ProviderEntry{Driver: "openai"},
			explicit: []string{"local-model"},
			want:     []string{"local-model"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, providerModels(tt.entry, tt.explicit))
		})
	}
}

// The local chat runs on the vendor's onboarding pick when that model is
// actually enabled, and on the daemon's default otherwise. The rule is
// unconditional — there is no "am I onboarding" signal to read.
func TestOnboardingModel(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.Config
		want string
	}{
		{
			name: "no config at all",
			cfg:  &config.Config{},
			want: "",
		},
		{
			name: "the recommendation is enabled",
			cfg: withConfig(
				map[string]config.ProviderEntry{"work": {Driver: "anthropic"}},
				"claude-opus-5", "claude-sonnet-5",
			),
			want: "claude-sonnet-5",
		},
		{
			name: "the recommendation is not enabled, so the default wins",
			cfg: withConfig(
				map[string]config.ProviderEntry{"work": {Driver: "anthropic"}},
				"claude-opus-5",
			),
			want: "claude-opus-5",
		},
		{
			name: "a provider with no recommendation falls through to the default",
			cfg: withConfig(
				map[string]config.ProviderEntry{"local": {Driver: "openai"}},
				"some-local-model",
			),
			want: "some-local-model",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, onboardingModel(tt.cfg))
		})
	}
}

func withConfig(providers map[string]config.ProviderEntry, modelIDs ...string) *config.Config {
	models := make([]config.ModelEntry, 0, len(modelIDs))
	for _, id := range modelIDs {
		models = append(models, config.ModelEntry{ID: id, Provider: "work"})
	}

	return &config.Config{UnifiedConfig: &config.UnifiedConfig{Providers: providers, Models: models}}
}
