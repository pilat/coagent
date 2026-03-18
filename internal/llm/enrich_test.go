package llm

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/catalog"
	"github.com/pilat/coagent/internal/config"
)

var _ driverProtocol = (*stubDriver)(nil)

type stubDriver struct {
	models map[string]catalog.ModelSpec
	err    error
	calls  int
}

func (d *stubDriver) NewClient(config.ProviderEntry, config.ModelEntry) (Client, error) {
	return nil, nil
}

func (d *stubDriver) ListModels(
	context.Context, string, config.ProviderEntry,
) (map[string]catalog.ModelSpec, error) {
	d.calls++

	return d.models, d.err
}

func TestEnrichCatalogFillsEntries(t *testing.T) {
	driver := &stubDriver{models: map[string]catalog.ModelSpec{
		"claude-opus-5": {
			Name:          "Claude Opus 5",
			Source:        "anthropic",
			ContextWindow: 1_000_000,
			MaxTokens:     128_000,
			Pricing:       &config.ModelPricing{InputPrice: 5, OutputPrice: 25},
			Reasoning:     &config.ReasoningSpec{Supported: true, NativeEffort: true},
		},
		"claude-haiku-4-5-20251001": {Name: "Claude Haiku 4.5", ContextWindow: 200_000, MaxTokens: 64_000},
	}}

	cfg := testConfig([]config.ModelEntry{
		{ID: "claude-opus-5", Provider: "prod"},
		{ID: "claude-haiku-4-5", Provider: "prod"},
	})

	require.NoError(t, enrichCatalog(context.Background(), cfg, map[string]driverProtocol{driverAnthropic: driver}))
	assert.Equal(t, 1, driver.calls, "one ListModels call per provider")

	opus := cfg.UnifiedConfig.Models[0]
	assert.Equal(t, "Claude Opus 5", opus.Name)
	assert.Equal(t, "prod/Claude Opus 5", opus.DisplayName)
	assert.Equal(t, 1_000_000, opus.ContextWindow)
	assert.Equal(t, 128_000, opus.MaxTokens)
	assert.InDelta(t, 5.0, opus.Pricing.InputPrice, 1e-9)
	assert.True(t, opus.Reasoning.Supported)

	haiku := cfg.UnifiedConfig.Models[1]
	assert.Equal(t, "prod/Claude Haiku 4.5", haiku.DisplayName, "date-normalized match still enriches")
}

// The picker builds its buttons from these levels, so the list must mean "the API
// accepts exactly these", not merely "the model can reason".
func TestEnrichCatalogFillsEffortLevelsPerDriver(t *testing.T) {
	tests := []struct {
		name        string
		driverName  string
		spec        *config.ReasoningSpec
		want        []string
		wantDefault string
	}{
		{
			name:       "anthropic native effort takes the catalog allowlist",
			driverName: driverAnthropic,
			spec: &config.ReasoningSpec{
				Supported: true, NativeEffort: true, Default: "max",
				Efforts: []string{"low", "high", "max"},
			},
			want:        []string{"low", "high", "max"},
			wantDefault: "max",
		},
		{
			name:        "anthropic budget model still offers the levels we map",
			driverName:  driverAnthropic,
			spec:        &config.ReasoningSpec{Supported: true, BudgetMin: 1024},
			want:        []string{"low", "medium", "high"},
			wantDefault: "medium",
		},
		{
			name:       "openrouter allowlist without medium picks a middle default",
			driverName: driverOpenRouter,
			spec: &config.ReasoningSpec{
				Supported: true, NativeEffort: true,
				Efforts: []string{"high", "xhigh"},
			},
			want:        []string{"high", "xhigh"},
			wantDefault: "xhigh",
		},
		{
			name:       "a catalog default of none is not pre-selected",
			driverName: driverOpenRouter,
			spec: &config.ReasoningSpec{
				Supported: true, NativeEffort: true, Default: "none",
				Efforts: []string{"none", "low", "medium"},
			},
			want:        []string{"none", "low", "medium"},
			wantDefault: "medium",
		},
		{
			name:       "openrouter model with no selector offers nothing",
			driverName: driverOpenRouter,
			spec:       &config.ReasoningSpec{Supported: true},
		},
		{
			name:        "openrouter model accepting any level gets the safe subset",
			driverName:  driverOpenRouter,
			spec:        &config.ReasoningSpec{Supported: true, NativeEffort: true, AnyEffort: true},
			want:        []string{"low", "medium", "high"},
			wantDefault: "medium",
		},
		{
			name:       "google-sa never sends a level",
			driverName: driverGoogleSA,
			spec:       &config.ReasoningSpec{Supported: true, NativeEffort: true, Efforts: []string{"low", "high"}},
		},
		{
			name:       "plain openai never sends a level",
			driverName: driverOpenAI,
			spec:       &config.ReasoningSpec{Supported: true, NativeEffort: true, Efforts: []string{"low", "high"}},
		},
		{
			name:       "a model that does not reason",
			driverName: driverAnthropic,
			spec:       &config.ReasoningSpec{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			driver := &stubDriver{models: map[string]catalog.ModelSpec{
				"m": {Name: "M", ContextWindow: 200_000, MaxTokens: 64_000, Reasoning: tt.spec},
			}}

			cfg := testConfig([]config.ModelEntry{{ID: "m", Provider: "prod"}})
			cfg.UnifiedConfig.Providers["prod"] = config.ProviderEntry{Driver: tt.driverName}

			require.NoError(
				t,
				enrichCatalog(context.Background(), cfg, map[string]driverProtocol{tt.driverName: driver}),
			)

			assert.Equal(t, tt.spec.Supported, cfg.UnifiedConfig.Models[0].Reasoning.Supported,
				"the catalog fact stays truthful")
			assert.Equal(t, tt.want, cfg.UnifiedConfig.Models[0].EffortLevels)
			assert.Equal(t, tt.wantDefault, cfg.UnifiedConfig.Models[0].DefaultEffort)
		})
	}
}

func TestEnrichCatalogDisplayNameFallsBackToID(t *testing.T) {
	driver := &stubDriver{models: map[string]catalog.ModelSpec{
		"local-model": {ContextWindow: 32_000},
	}}

	cfg := testConfig([]config.ModelEntry{{ID: "local-model", Provider: "prod"}})
	cfg.UnifiedConfig.Providers["prod"] = config.ProviderEntry{Driver: driverOpenAI}

	require.NoError(t, enrichCatalog(context.Background(), cfg, map[string]driverProtocol{driverOpenAI: driver}))
	assert.Equal(t, "local-model", cfg.UnifiedConfig.Models[0].Name)
	assert.Equal(t, "local-model", cfg.UnifiedConfig.Models[0].DisplayName)
}

func TestEnrichCatalogListsEveryUnresolvedModel(t *testing.T) {
	driver := &stubDriver{models: map[string]catalog.ModelSpec{
		"known": {Name: "Known", ContextWindow: 1000, MaxTokens: 100},
	}}

	cfg := testConfig([]config.ModelEntry{
		{ID: "known", Provider: "prod"},
		{ID: "ghost-one", Provider: "prod"},
		{ID: "ghost-two", Provider: "prod"},
	})

	err := enrichCatalog(context.Background(), cfg, map[string]driverProtocol{driverAnthropic: driver})
	require.Error(t, err)
	require.ErrorContains(t, err, "ghost-one")
	require.ErrorContains(t, err, "ghost-two")
	assert.NotContains(t, err.Error(), "known")
}

func TestEnrichCatalogValidatesResolvedMetadata(t *testing.T) {
	tests := []struct {
		name       string
		driverName string
		modelID    string
		spec       catalog.ModelSpec
		wantErr    string
	}{
		{
			name:       "missing context window is fatal for every driver",
			driverName: driverOpenAI,
			modelID:    "some-model",
			spec:       catalog.ModelSpec{Name: "Some", MaxTokens: 4096},
			wantErr:    "context window",
		},
		{
			name:       "anthropic driver demands max tokens",
			driverName: driverAnthropic,
			modelID:    "claude-opus-5",
			spec:       catalog.ModelSpec{Name: "Opus", ContextWindow: 200_000},
			wantErr:    "max output tokens",
		},
		{
			name:       "anthropic-prefixed id demands max tokens on any driver",
			driverName: driverOpenAI,
			modelID:    "anthropic/claude-opus-5",
			spec:       catalog.ModelSpec{Name: "Opus", ContextWindow: 200_000},
			wantErr:    "max output tokens",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			driver := &stubDriver{models: map[string]catalog.ModelSpec{tt.modelID: tt.spec}}

			cfg := testConfig([]config.ModelEntry{{ID: tt.modelID, Provider: "prod"}})
			cfg.UnifiedConfig.Providers["prod"] = config.ProviderEntry{Driver: tt.driverName}

			err := enrichCatalog(context.Background(), cfg, map[string]driverProtocol{tt.driverName: driver})
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestEnrichCatalogAllowsMissingMaxTokensElsewhere(t *testing.T) {
	driver := &stubDriver{models: map[string]catalog.ModelSpec{
		"local-model": {Name: "Local", ContextWindow: 32_000},
	}}

	cfg := testConfig([]config.ModelEntry{{ID: "local-model", Provider: "prod"}})
	cfg.UnifiedConfig.Providers["prod"] = config.ProviderEntry{Driver: driverOpenAI}

	assert.NoError(t, enrichCatalog(context.Background(), cfg, map[string]driverProtocol{driverOpenAI: driver}))
}

func TestEnrichCatalogFailsFetch(t *testing.T) {
	driver := &stubDriver{err: assert.AnError}

	cfg := testConfig([]config.ModelEntry{{ID: "claude-opus-5", Provider: "prod"}})

	assert.ErrorContains(t,
		enrichCatalog(context.Background(), cfg, map[string]driverProtocol{driverAnthropic: driver}),
		"catalog unavailable")
}

// One dead catalog must not hide what else is broken — startup fails once either
// way, so the operator should see every problem in a single run.
func TestEnrichCatalogReportsEveryBrokenProvider(t *testing.T) {
	dead := &stubDriver{err: assert.AnError}
	alive := &stubDriver{models: map[string]catalog.ModelSpec{}}

	cfg := testConfig([]config.ModelEntry{
		{ID: "m-a", Provider: "aaa"},
		{ID: "m-z", Provider: "zzz"},
	})
	cfg.UnifiedConfig.Providers["aaa"] = config.ProviderEntry{Driver: driverAnthropic}
	cfg.UnifiedConfig.Providers["zzz"] = config.ProviderEntry{Driver: driverOpenAI}

	err := enrichCatalog(context.Background(), cfg, map[string]driverProtocol{
		driverAnthropic: dead,
		driverOpenAI:    alive,
	})
	require.ErrorContains(t, err, "aaa")
	require.ErrorContains(t, err, "m-z", "a later provider's own problems still surface")
}

func TestEnrichCatalogUnknownDriver(t *testing.T) {
	cfg := testConfig([]config.ModelEntry{{ID: "m", Provider: "prod"}})
	cfg.UnifiedConfig.Providers["prod"] = config.ProviderEntry{Driver: "made-up"}

	assert.ErrorContains(t,
		enrichCatalog(context.Background(), cfg, map[string]driverProtocol{}),
		"unsupported driver")
}

func TestEnrichCatalogNoModels(t *testing.T) {
	assert.NoError(t, enrichCatalog(context.Background(), &config.Config{}, nil))
	assert.NoError(t, enrichCatalog(context.Background(), testConfig(nil), nil))
}

func testConfig(models []config.ModelEntry) *config.Config {
	return &config.Config{
		UnifiedConfig: &config.UnifiedConfig{
			Providers: map[string]config.ProviderEntry{"prod": {Driver: driverAnthropic}},
			Models:    models,
		},
	}
}
