package llm

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/catalog"
	"github.com/pilat/coagent/internal/config"
)

const sectionGoneBody = `{"other": {"models": {}}}`

func TestEnrichCatalogCarriesAModelWhoseSectionIsGone(t *testing.T) {
	for _, tc := range []struct {
		section string
		entry   config.ProviderEntry
	}{
		{sectionAnthropic, config.ProviderEntry{Driver: driverAnthropic}},
		{sectionGoogleVertex, config.ProviderEntry{Driver: driverGoogleSA}},
		{"acme", config.ProviderEntry{Driver: driverOpenAI, Catalog: "acme"}},
	} {
		t.Run(tc.entry.Driver, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			logs := captureWarnings(t)
			f := &fakeFetcher{bodies: map[string]string{modelsDevURL: fmt.Sprintf(
				`{%q: {"models": {"m": {"name": "M", "limit": {"context": 1000, "output": 100}}}}}`, tc.section)}}
			first, err := enrichSection(f, tc.entry)
			require.NoError(t, err)
			f.bodies[modelsDevURL] = sectionGoneBody
			for range 2 {
				logs.TakeAll()
				last, err := enrichSection(f, tc.entry)
				require.NoError(t, err)
				assert.True(t, last.Models[0].CatalogStale)
				last.Models[0].CatalogStale = false
				assert.Equal(t, first.Models[0], last.Models[0])
				assert.Equal(t, 1, logs.FilterMessage("catalog_model_missing_upstream").Len())
			}
		})
	}
}

func TestEnrichCatalogLostSectionWithoutResolutionStaysFatal(t *testing.T) {
	pinned := config.ProviderEntry{Driver: driverOpenAI, Catalog: "acme"}
	for name, resolvedFirst := range map[string]bool{"never resolved": false, "re-pinned to a typo": true} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			f := &fakeFetcher{bodies: map[string]string{
				modelsDevURL: `{"acme": {"models": {"m": {"limit": {"context": 1000}}}}}`,
			}}
			if resolvedFirst {
				_, err := enrichSection(f, pinned)
				require.NoError(t, err)
			}
			logs := captureWarnings(t)
			typo := pinned
			typo.Catalog = "acme-typo"
			_, err := enrichSection(f, typo)
			require.EqualError(t, err, "model catalog resolution failed:\n"+
				"  - provider \"p\": catalog unavailable: models.dev has no section \"acme-typo\"")
			assert.Zero(t, logs.Len())
		})
	}
}

func TestEnrichCatalogLostSectionWithANeverResolvedModelStaysFatal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	pinned := config.ProviderEntry{Driver: driverOpenAI, Catalog: "acme"}
	f := &fakeFetcher{bodies: map[string]string{
		modelsDevURL: `{"acme": {"models": {"m": {"limit": {"context": 1000}}}}}`,
	}}
	_, err := enrichSection(f, pinned)
	require.NoError(t, err)
	before := rememberedSection(t, "m", pinned)
	f.bodies[modelsDevURL] = sectionGoneBody
	_, err = enrichSection(f, pinned, "m", "n")
	require.EqualError(t, err, "model catalog resolution failed:\n"+
		"  - provider \"p\": catalog unavailable: models.dev has no section \"acme\"")
	assert.True(t, before.ResolvedAt.Equal(rememberedSection(t, "m", pinned).ResolvedAt))
	_, known := catalog.LoadResolutions(context.Background()).Recall("n", pinned)
	assert.False(t, known)
}

func rememberedSection(t *testing.T, id string, entry config.ProviderEntry) catalog.Resolution {
	t.Helper()
	last, ok := catalog.LoadResolutions(context.Background()).Recall(id, entry)
	require.True(t, ok)
	return last
}

func enrichSection(f *fakeFetcher, entry config.ProviderEntry, ids ...string) (*config.UnifiedConfig, error) {
	if len(ids) == 0 {
		ids = []string{"m"}
	}

	models := make([]config.ModelEntry, len(ids))
	for i, id := range ids {
		models[i] = config.ModelEntry{ID: id, Provider: "p"}
	}

	cfg := &config.Config{UnifiedConfig: &config.UnifiedConfig{
		Providers: map[string]config.ProviderEntry{"p": entry},
		Models:    models,
	}}
	ctx := context.Background()
	err := enrichWithResolutions(ctx, cfg, newDrivers(f), catalog.LoadResolutions(ctx))

	return cfg.UnifiedConfig, err
}
