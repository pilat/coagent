package catalog

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecommend(t *testing.T) {
	tests := []struct {
		section string
		want    Recommendation
		ok      bool
	}{
		{
			section: "anthropic",
			want:    Recommendation{Default: "claude-sonnet-5", Onboarding: "claude-sonnet-5"},
			ok:      true,
		},
		{
			section: "openrouter",
			want:    Recommendation{Default: "anthropic/claude-sonnet-5", Onboarding: "anthropic/claude-sonnet-5"},
			ok:      true,
		},
		{
			section: "google-vertex",
			want:    Recommendation{Default: "gemini-3.5-flash", Onboarding: "gemini-3.5-flash"},
			ok:      true,
		},
		{
			// A bare openai endpoint has no vendor to key on: the caller has to ask
			// for a model id rather than guess one.
			section: "",
			ok:      false,
		},
		{
			section: "some-local-llama-host",
			ok:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.section, func(t *testing.T) {
			got, ok := Recommend(tt.section)
			require.Equal(t, tt.ok, ok)

			if tt.ok {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

// The default has to come first: the config's model list order *is* the default,
// so a recommendation that enabled them the other way round would start sessions
// on the onboarding model forever.
func TestRecommendation_ModelsPutsTheDefaultFirst(t *testing.T) {
	tests := []struct {
		name string
		rec  Recommendation
		want []string
	}{
		{
			name: "same model for both roles is enabled once",
			rec:  Recommendation{Default: "m", Onboarding: "m"},
			want: []string{"m"},
		},
		{
			name: "a distinct onboarding model follows the default",
			rec:  Recommendation{Default: "big", Onboarding: "small"},
			want: []string{"big", "small"},
		},
		{
			name: "no onboarding pick is just the default",
			rec:  Recommendation{Default: "m"},
			want: []string{"m"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.rec.Models())
		})
	}
}

// Every recommended id must be shaped like a model a provider would accept.
// Nothing here reaches the network: a recommendation that only resolves online
// would leave a first run with no way to start.
func TestRecommendations_AreResolvableWithoutTheNetwork(t *testing.T) {
	for section, rec := range recommendations {
		assert.NotEmpty(t, rec.Default, section)
		assert.NotEmpty(t, rec.Onboarding, section)

		for _, id := range rec.Models() {
			assert.NotContains(t, id, " ", section)
			assert.NotContains(t, id, ":free", section+" must not recommend a free tier that rate-limits a first run")
		}
	}
}
