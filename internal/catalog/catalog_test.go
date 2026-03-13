package catalog

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
)

func TestLookup(t *testing.T) {
	models := map[string]ModelSpec{
		"claude-opus-5":            {Name: "undated"},
		"claude-opus-4-5@20251101": {Name: "vertex dated"},
		"claude-haiku-4-5-2025100": {Name: "seven digits, not a date"},
		"qwen3:14b":                {Name: "colon in id"},
	}

	tests := []struct {
		name  string
		id    string
		want  string
		found bool
	}{
		{"exact", "claude-opus-5", "undated", true},
		{"exact with colon", "qwen3:14b", "colon in id", true},
		{"dated config id against undated catalog key", "claude-opus-5-20260101", "undated", true},
		{"undated config id against @-dated catalog key", "claude-opus-4-5", "vertex dated", true},
		{"@-dated config id against @-dated catalog key", "claude-opus-4-5@20260202", "vertex dated", true},
		{"short numeric suffix is not a date", "claude-haiku-4-5", "", false},
		{"absent", "gpt-5", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, ok := Lookup(models, tt.id)
			assert.Equal(t, tt.found, ok)
			assert.Equal(t, tt.want, spec.Name)
		})
	}
}

func TestLookupNormalizedTieIsStable(t *testing.T) {
	models := map[string]ModelSpec{
		"model-a-20240101": {Name: "first"},
		"model-a-20250101": {Name: "second"},
	}

	spec, ok := Lookup(models, "model-a")
	require.True(t, ok)

	for range 20 {
		again, _ := Lookup(models, "model-a")
		assert.Equal(t, spec.Name, again.Name)
	}
}

func TestFlattenPrefersSortedSectionOrder(t *testing.T) {
	sections := map[string]map[string]ModelSpec{
		"zai":      {"shared": {Name: "from zai"}, "glm-5": {Name: "glm"}},
		"deepseek": {"shared": {Name: "from deepseek"}},
	}

	merged := Flatten(sections)
	assert.Equal(t, "from deepseek", merged["shared"].Name)
	assert.Equal(t, "glm", merged["glm-5"].Name)
}

// Open-weight ids are served by many hosts with different windows and prices, and
// the winner feeds the compaction trigger and cost accounting. Pin which one wins.
func TestFlattenDuplicateIDResolvesToAlphabeticallyFirstSection(t *testing.T) {
	sections := map[string]map[string]ModelSpec{
		"together": {"llama-4-70b": {
			Name: "from together", Source: "together",
			ContextWindow: 131_072,
			Pricing:       &config.ModelPricing{InputPrice: 0.88, OutputPrice: 0.88},
		}},
		"groq": {"llama-4-70b": {
			Name: "from groq", Source: "groq",
			ContextWindow: 8_192,
			Pricing:       &config.ModelPricing{InputPrice: 0.59, OutputPrice: 0.79},
		}},
		"fireworks-ai": {
			"llama-4-70b": {
				Name: "from fireworks", Source: "fireworks-ai",
				ContextWindow: 262_144,
				Pricing:       &config.ModelPricing{InputPrice: 0.9, OutputPrice: 0.9},
			},
			"fireworks-only": {Name: "unique", Source: "fireworks-ai", ContextWindow: 1_000},
		},
	}

	merged := Flatten(sections)

	won := merged["llama-4-70b"]
	assert.Equal(t, "fireworks-ai", won.Source)
	assert.Equal(t, 262_144, won.ContextWindow)
	assert.InDelta(t, 0.9, won.Pricing.InputPrice, 1e-9)
	assert.Equal(t, []string{"groq", "together"}, won.Shadowed, "losers listed in sorted order")

	assert.Nil(t, merged["fireworks-only"].Shadowed, "a unique id is not ambiguous")

	t.Run("stable across restarts", func(t *testing.T) {
		for range 50 {
			again := Flatten(sections)
			assert.Equal(t, "fireworks-ai", again["llama-4-70b"].Source)
			assert.Equal(t, 262_144, again["llama-4-70b"].ContextWindow)
			assert.Equal(t, []string{"groq", "together"}, again["llama-4-70b"].Shadowed)
		}
	})

	t.Run("date-normalized lookup keeps the same winner", func(t *testing.T) {
		spec, ok := Lookup(merged, "llama-4-70b-20260101")
		require.True(t, ok)
		assert.Equal(t, "fireworks-ai", spec.Source)
		assert.Equal(t, 262_144, spec.ContextWindow)
		assert.Equal(t, []string{"groq", "together"}, spec.Shadowed)
	})
}

// Flatten must not mutate the sections it merges: the shadow list is a property of
// the merge, and a second Flatten would otherwise accumulate duplicates.
func TestFlattenLeavesSectionsUntouched(t *testing.T) {
	sections := map[string]map[string]ModelSpec{
		"aaa": {"dup": {Source: "aaa"}},
		"bbb": {"dup": {Source: "bbb"}},
	}

	Flatten(sections)
	Flatten(sections)

	assert.Nil(t, sections["aaa"]["dup"].Shadowed)
	assert.Equal(t, []string{"bbb"}, Flatten(sections)["dup"].Shadowed)
}

func TestCacheNameIsStablePerURL(t *testing.T) {
	a := CacheName("openrouter", "https://openrouter.ai/api/v1/models")
	b := CacheName("openrouter", "https://proxy.local/v1/models")

	assert.NotEqual(t, a, b)
	assert.Equal(t, a, CacheName("openrouter", "https://openrouter.ai/api/v1/models"))
	assert.Len(t, a, len("openrouter-")+8+len(".json"))
}
