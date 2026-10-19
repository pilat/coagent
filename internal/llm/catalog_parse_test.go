package llm

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/catalog"
	"github.com/pilat/coagent/internal/config"
)

func TestParseModelsDev(t *testing.T) {
	sections, err := parseModelsDev(fixture(t, "modelsdev.json"))
	require.NoError(t, err)
	require.Contains(t, sections, "anthropic")

	anthropic := sections["anthropic"]

	t.Run("limits, cost and source", func(t *testing.T) {
		spec := anthropic["claude-opus-5"]
		assert.Equal(t, "Claude Opus 5", spec.Name)
		assert.Equal(t, "anthropic", spec.Source)
		assert.Equal(t, 1000000, spec.ContextWindow)
		assert.Equal(t, 128000, spec.MaxTokens)
		assert.Equal(t, &config.ModelPricing{
			InputPrice: 5, OutputPrice: 25, CacheReadPrice: 0.5, CacheWritePrice: 6.25,
		}, spec.Pricing)
	})

	t.Run("input modalities", func(t *testing.T) {
		// Provenance: live models.dev capture (2026-08) carries per-model
		// `modalities: {"input": [...], "output": [...]}` on the section's
		// models; e.g. anthropic ⇒ input ["text","image"]. The fixture pins that
		// shape; a drift here must surface as this assertion, not a dead gate.
		assert.Equal(t, []string{"text", "image"}, anthropic["claude-opus-5"].InputModalities)
		assert.Equal(t, []string{"text"}, anthropic["claude-legacy-2"].InputModalities)

		// Absent key decodes nil — every capability gate fails closed.
		assert.Nil(t, anthropic["claude-nameless"].InputModalities)
	})

	t.Run("model without cost carries no pricing", func(t *testing.T) {
		assert.Nil(t, anthropic["claude-thinker-no-options"].Pricing)
	})

	reasoning := []struct {
		name string
		id   string
		want config.ReasoningSpec
	}{
		{
			"effort only",
			"claude-opus-5",
			config.ReasoningSpec{Supported: true, NativeEffort: true, Efforts: []string{"low", "medium", "high"}},
		},
		{"budget only", "claude-haiku-4-5", config.ReasoningSpec{Supported: true, BudgetMin: 2048}},
		{
			"both",
			"claude-sonnet-4-6",
			config.ReasoningSpec{
				Supported: true, NativeEffort: true, BudgetMin: 1024,
				Efforts: []string{"low", "medium", "high"},
			},
		},
		{
			"reasoning without options defaults the budget floor",
			"claude-thinker-no-options",
			config.ReasoningSpec{Supported: true, BudgetMin: defaultBudgetMin},
		},
		{"no reasoning", "claude-legacy-2", config.ReasoningSpec{}},
	}

	for _, tt := range reasoning {
		t.Run("reasoning: "+tt.name, func(t *testing.T) {
			assert.Equal(t, &tt.want, anthropic[tt.id].Reasoning)
		})
	}
}

func TestParseModelsDevRejectsGarbage(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"not json", "<html>503</html>"},
		{"empty object", "{}"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseModelsDev([]byte(tt.body))
			assert.Error(t, err)
		})
	}
}

// TestModelsDevFixtureModalityGuard fails when the fixture no longer feeds the
// modality decoder: every vision gate test would then pass trivially (nil =
// fail-closed) over a dead feature. Upstream shape drift on models.dev must
// break THIS test, not silently green the gates.
func TestModelsDevFixtureModalityGuard(t *testing.T) {
	sections, err := parseModelsDev(fixture(t, "modelsdev.json"))
	require.NoError(t, err)

	assert.False(t, specsAllNil(sections),
		"no modelsdev fixture model decodes modalities.input — vision gates test nothing")
}

// TestOpenRouterFixtureModalityGuard is the arrow-form counterpart: the fixture
// must still decode architecture.modality or the parsing below tests nothing.
func TestOpenRouterFixtureModalityGuard(t *testing.T) {
	models, err := parseOpenRouter(fixture(t, "openrouter.json"))
	require.NoError(t, err)

	bySpec := map[string]map[string]catalog.ModelSpec{"openrouter": models}
	assert.False(t, specsAllNil(bySpec),
		"no openrouter fixture model decodes architecture.modality — vision gates test nothing")
}

func specsAllNil(sections map[string]map[string]catalog.ModelSpec) bool {
	for _, models := range sections {
		for _, spec := range models {
			if spec.InputModalities != nil {
				return false
			}
		}
	}

	return true
}

func TestParseOpenRouter(t *testing.T) {
	models, err := parseOpenRouter(fixture(t, "openrouter.json"))
	require.NoError(t, err)

	t.Run("per-token string prices scale to per-million", func(t *testing.T) {
		spec := models["anthropic/claude-opus-5"]
		assert.Equal(t, "Anthropic: Claude Opus 5", spec.Name)
		assert.Equal(t, "openrouter", spec.Source)
		assert.InDelta(t, 5.0, spec.Pricing.InputPrice, 1e-9)
		assert.InDelta(t, 25.0, spec.Pricing.OutputPrice, 1e-9)
		assert.InDelta(t, 0.5, spec.Pricing.CacheReadPrice, 1e-9)
		assert.InDelta(t, 6.25, spec.Pricing.CacheWritePrice, 1e-9)
	})

	t.Run("top_provider context wins over the model-level maximum", func(t *testing.T) {
		assert.Equal(t, 900000, models["anthropic/claude-opus-5"].ContextWindow)
		assert.Equal(t, 128000, models["anthropic/claude-opus-5"].MaxTokens)
	})

	t.Run("a window-sized output limit is reported verbatim", func(t *testing.T) {
		spec := models["moonshotai/kimi-k2.5"]
		assert.Equal(t, 262144, spec.ContextWindow)
		assert.Equal(t, 262144, spec.MaxTokens)
	})

	t.Run("null max_completion_tokens leaves max tokens unset", func(t *testing.T) {
		assert.Equal(t, 0, models["openai/gpt-5-mini"].MaxTokens)
	})

	t.Run("missing cache prices are zero", func(t *testing.T) {
		assert.Zero(t, models["openai/gpt-5-mini"].Pricing.CacheReadPrice)
		assert.Zero(t, models["openai/gpt-5-mini"].Pricing.CacheWritePrice)
	})

	reasoning := []struct {
		name string
		id   string
		want config.ReasoningSpec
	}{
		{
			"allowlist is normalized to weakest-first",
			"anthropic/claude-opus-5",
			config.ReasoningSpec{
				Supported: true, NativeEffort: true, Default: "high",
				Efforts: []string{"low", "medium", "high", "max"},
			},
		},
		{
			"narrow allowlist survives verbatim",
			"zai/glm-narrow",
			config.ReasoningSpec{
				Supported: true, NativeEffort: true, Default: "none",
				Efforts: []string{"high", "xhigh"},
			},
		},
		{
			"absent supported_efforts means no selector",
			"minimax/no-selector",
			config.ReasoningSpec{Supported: true},
		},
		{
			"null supported_efforts means every level",
			"vendor/any-effort",
			config.ReasoningSpec{Supported: true, NativeEffort: true, AnyEffort: true},
		},
		{
			"router without a reasoning object falls back to supported_parameters",
			"openrouter/auto",
			config.ReasoningSpec{Supported: true},
		},
		{"no reasoning at all", "openai/gpt-5-mini", config.ReasoningSpec{}},
	}

	for _, tt := range reasoning {
		t.Run("reasoning: "+tt.name, func(t *testing.T) {
			assert.Equal(t, &tt.want, models[tt.id].Reasoning)
		})
	}

	t.Run("arrow-form modality parses the input side", func(t *testing.T) {
		// Provenance: live OpenRouter capture exposes
		// `architecture.modality: "text->text"` (testdata/openrouter.json);
		// multi-modal models use "+"-joined inputs. The fixture pins both.
		assert.Equal(t, []string{"text", "image"}, models["anthropic/claude-opus-5"].InputModalities)
		assert.Equal(t, []string{"text"}, models["openai/gpt-5-mini"].InputModalities)

		// Absent or unparseable modality decodes nil — fail closed.
		assert.Nil(t, models["zai/glm-narrow"].InputModalities)
	})
}

func TestParseOpenRouterRejectsGarbage(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"not json", "nope"},
		{"empty list", `{"data": []}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseOpenRouter([]byte(tt.body))
			assert.Error(t, err)
		})
	}
}

func TestParseOpenRouterModality(t *testing.T) {
	tests := []struct {
		raw  string
		want []string
	}{
		{"text->text", []string{"text"}},
		{"text+image->text+image", []string{"text", "image"}},
		{"", nil},
		{"garbage", nil},
		{"->text", nil},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, parseOpenRouterModality(tt.raw), "raw=%q", tt.raw)
	}
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()

	body, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)

	return body
}
