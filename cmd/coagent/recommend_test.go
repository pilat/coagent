package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecommend(t *testing.T) {
	t.Parallel()

	tests := []struct {
		section string
		want    recommendation
		ok      bool
	}{
		{
			section: "anthropic",
			want:    recommendation{Default: "claude-sonnet-5", Onboarding: "claude-sonnet-5"},
			ok:      true,
		},
		{
			section: "openrouter",
			want:    recommendation{Default: "anthropic/claude-sonnet-5", Onboarding: "anthropic/claude-sonnet-5"},
			ok:      true,
		},
		{
			section: "google-vertex",
			want:    recommendation{Default: "gemini-3.5-flash", Onboarding: "gemini-3.5-flash"},
			ok:      true,
		},
		{
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
			t.Parallel()

			got, ok := recommend(tt.section)
			require.Equal(t, tt.ok, ok)

			if tt.ok {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestRecommendationModelsPutsTheDefaultFirst(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		rec  recommendation
		want []string
	}{
		{
			name: "same model for both roles is enabled once",
			rec:  recommendation{Default: "m", Onboarding: "m"},
			want: []string{"m"},
		},
		{
			name: "a distinct onboarding model follows the default",
			rec:  recommendation{Default: "big", Onboarding: "small"},
			want: []string{"big", "small"},
		},
		{
			name: "no onboarding pick is just the default",
			rec:  recommendation{Default: "m"},
			want: []string{"m"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, tt.rec.models())
		})
	}
}

func TestRecommendationsAreResolvableWithoutTheNetwork(t *testing.T) {
	t.Parallel()

	for section, rec := range recommendations {
		assert.NotEmpty(t, rec.Default, section)
		assert.NotEmpty(t, rec.Onboarding, section)

		for _, id := range rec.models() {
			assert.NotContains(t, id, " ", section)
			assert.NotContains(t, id, ":free", section+" must not recommend a free tier that rate-limits a first run")
		}
	}
}
