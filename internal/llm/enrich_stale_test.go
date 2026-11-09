package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/catalog"
	"github.com/pilat/coagent/internal/config"
)

func TestEnrichCatalogCarriesLastResolution(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	logs := captureWarnings(t)
	driver := staleTestDriver()
	first, err := boot(t, []string{"a"}, driver)
	require.NoError(t, err)
	driver.models = nil
	second, err := boot(t, []string{"a"}, driver)
	require.NoError(t, err)
	want, got := first.UnifiedConfig.Models[0], second.UnifiedConfig.Models[0]
	assert.False(t, want.CatalogStale)
	assert.True(t, got.CatalogStale)
	got.CatalogStale = false
	assert.Equal(t, want, got)
	warns := logs.FilterMessage("catalog_model_missing_upstream").All()
	require.Len(t, warns, 1)
	assert.Equal(t, "a", warns[0].ContextMap()["id"])
	assert.Equal(t, "prod", warns[0].ContextMap()["provider"])
	assert.Positive(t, warns[0].ContextMap()["age"].(time.Duration))
	assert.Zero(t, logs.FilterMessage("catalog_section_ambiguous").Len())
}

func TestEnrichCatalogFailsOnNeverResolvedModel(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	logs := captureWarnings(t)
	_, err := boot(t, []string{"typo"}, staleTestDriver())
	require.EqualError(
		t,
		err,
		"model catalog resolution failed:\n  - typo (provider \"prod\"): not present in its catalog",
	)
	assert.Zero(t, logs.Len())
}

func TestEnrichCatalogSilentWhenCatalogHasTheModel(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	logs := captureWarnings(t)
	for range 2 {
		cfg, err := boot(t, []string{"a"}, staleTestDriver())
		require.NoError(t, err)
		assert.False(t, cfg.UnifiedConfig.Models[0].CatalogStale)
	}
	assert.Zero(t, logs.Len())
}

func TestEnrichCatalogWarnsEveryBootAndKeepsResolvedAt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	logs := captureWarnings(t)
	driver := staleTestDriver()
	_, err := boot(t, []string{"a"}, driver)
	require.NoError(t, err)
	want := rememberedStaleModel(t, "a")
	driver.models = nil
	for range 2 {
		logs.TakeAll()
		_, err := boot(t, []string{"a"}, driver)
		require.NoError(t, err)
		assert.Equal(t, 1, logs.FilterMessage("catalog_model_missing_upstream").Len())
		assert.True(t, want.ResolvedAt.Equal(rememberedStaleModel(t, "a").ResolvedAt))
	}
}

func TestEnrichCatalogClearsStaleWhenTheModelReturns(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	logs := captureWarnings(t)
	_, err := boot(t, []string{"a"}, staleTestDriver())
	require.NoError(t, err)
	first := rememberedStaleModel(t, "a")
	_, err = boot(t, []string{"a"}, &stubDriver{})
	require.NoError(t, err)
	logs.TakeAll()
	last, err := boot(t, []string{"a"}, staleTestDriver())
	require.NoError(t, err)
	assert.False(t, last.UnifiedConfig.Models[0].CatalogStale)
	assert.Zero(t, logs.Len())
	assert.True(t, rememberedStaleModel(t, "a").ResolvedAt.After(first.ResolvedAt))
}

func TestEnrichCatalogValidatesACarriedResolution(t *testing.T) {
	memory := &catalog.Resolutions{}
	cfg := testConfig([]config.ModelEntry{{ID: "a", Provider: "prod"}})
	memory.Remember("a", cfg.UnifiedConfig.Providers["prod"], catalog.ModelSpec{Name: "x"})
	err := enrichWithResolutions(context.Background(), cfg,
		map[string]driverProtocol{driverAnthropic: &stubDriver{}}, memory)
	require.ErrorContains(t, err, ": catalog carries no context window")
}

func TestEnrichCatalogSavesNothingFromAFailedBoot(t *testing.T) {
	for _, failure := range []string{"typo", "invalid spec", "dead provider"} {
		t.Run(failure, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			driver := staleTestDriver()
			cfg := testConfig([]config.ModelEntry{{ID: "a", Provider: "prod"}, {ID: "bad", Provider: "prod"}})
			drivers := map[string]driverProtocol{driverAnthropic: driver}
			switch failure {
			case "invalid spec":
				driver.models["bad"] = catalog.ModelSpec{Name: "Bad", MaxTokens: 100}
			case "dead provider":
				cfg.UnifiedConfig.Models[1].Provider = "dead"
				cfg.UnifiedConfig.Providers["dead"] = config.ProviderEntry{Driver: driverOpenAI}
				drivers[driverOpenAI] = &stubDriver{err: assert.AnError}
			}
			ctx := context.Background()
			err := enrichWithResolutions(ctx, cfg, drivers, catalog.LoadResolutions(ctx))
			require.Error(t, err)
			_, known := catalog.LoadResolutions(ctx).Recall("a", cfg.UnifiedConfig.Providers["prod"])
			assert.False(t, known)
		})
	}
}

func TestEnrichCatalogRepointedProviderFailsClosed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	logs := captureWarnings(t)
	_, err := boot(t, []string{"a"}, staleTestDriver())
	require.NoError(t, err)
	cfg := testConfig([]config.ModelEntry{{ID: "a", Provider: "prod"}})
	cfg.UnifiedConfig.Providers["prod"] = config.ProviderEntry{Driver: driverAnthropic, BaseURL: "http://other.test"}
	ctx := context.Background()
	err = enrichWithResolutions(
		ctx,
		cfg,
		map[string]driverProtocol{driverAnthropic: &stubDriver{}},
		catalog.LoadResolutions(ctx),
	)
	require.EqualError(
		t,
		err,
		"model catalog resolution failed:\n  - a (provider \"prod\"): not present in its catalog",
	)
	assert.Zero(t, logs.Len())
}

func TestEnrichCatalogDeadCatalogStaysFatal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	logs := captureWarnings(t)
	driver := staleTestDriver()
	_, err := boot(t, []string{"a"}, driver)
	require.NoError(t, err)
	driver.err = assert.AnError
	_, err = boot(t, []string{"a"}, driver)
	require.ErrorContains(t, err, "provider \"prod\": catalog unavailable")
	assert.Zero(t, logs.FilterMessage("catalog_model_missing_upstream").Len())
}

func TestEnrichCatalogKeepsResolutionOfARemovedModel(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	driver := staleTestDriver()
	driver.models["b"] = driver.models["a"]
	_, err := boot(t, []string{"a", "b"}, driver)
	require.NoError(t, err)
	delete(driver.models, "b")
	_, err = boot(t, []string{"a"}, driver)
	require.NoError(t, err)
	last, err := boot(t, []string{"a", "b"}, driver)
	require.NoError(t, err)
	assert.True(t, last.UnifiedConfig.Models[1].CatalogStale)
}

func TestEnrichCatalogCarriesThroughAnOfflineCachedBody(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	logs := captureWarnings(t)
	var body atomic.Pointer[string]
	both := `{"data":[{"id":"a","context_length":1000},{"id":"b","context_length":1000}]}`
	body.Store(&both)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(*body.Load()))
	}))
	t.Cleanup(srv.Close)
	ctx := context.Background()
	for i := range 3 {
		cfg := testConfig([]config.ModelEntry{{ID: "a", Provider: "prod"}, {ID: "b", Provider: "prod"}})
		cfg.UnifiedConfig.Providers["prod"] = config.ProviderEntry{
			Driver:  driverOpenRouter,
			APIKey:  "k",
			BaseURL: srv.URL,
		}
		require.NoError(t, enrichWithResolutions(ctx, cfg, newDrivers(catalog.New()), catalog.LoadResolutions(ctx)))
		assert.Equal(t, i > 0, cfg.UnifiedConfig.Models[1].CatalogStale)
		one := `{"data":[{"id":"a","context_length":1000}]}`
		body.Store(&one)
		if i == 1 {
			srv.Close()
			logs.TakeAll()
		}
	}
	assert.Equal(t, 1, logs.FilterMessage("catalog_fetch_failed_using_cache").Len())
	assert.Equal(t, 1, logs.FilterMessage("catalog_model_missing_upstream").Len())
}

func boot(t *testing.T, ids []string, driver *stubDriver) (*config.Config, error) {
	t.Helper()
	models := make([]config.ModelEntry, len(ids))
	for i, id := range ids {
		models[i] = config.ModelEntry{ID: id, Provider: "prod"}
	}
	cfg := testConfig(models)
	ctx := context.Background()
	err := enrichWithResolutions(
		ctx,
		cfg,
		map[string]driverProtocol{driverAnthropic: driver},
		catalog.LoadResolutions(ctx),
	)
	return cfg, err
}

func staleTestDriver() *stubDriver {
	return &stubDriver{models: map[string]catalog.ModelSpec{"a": {
		Name:          "Model A",
		Source:        "anthropic",
		ContextWindow: 100000,
		MaxTokens:     4000,
		Pricing:       &config.ModelPricing{InputPrice: 1, OutputPrice: 2, CacheReadPrice: 3, CacheWritePrice: 4},
		Reasoning: &config.ReasoningSpec{
			Supported:    true,
			NativeEffort: true,
			Efforts:      []string{"low", "high"},
			Default:      "high",
		},
		InputModalities: []string{"text", "image"},
	}}}
}

func rememberedStaleModel(t *testing.T, id string) catalog.Resolution {
	t.Helper()
	last, ok := catalog.LoadResolutions(context.Background()).Recall(id, config.ProviderEntry{Driver: driverAnthropic})
	require.True(t, ok)
	return last
}
