package llm

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pilat/coagent/internal/catalog"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/logger"
)

// ambiguousSections is one open-weight id served by three hosts with different
// windows and prices, plus an id only one host carries.
func ambiguousSections() map[string]map[string]catalog.ModelSpec {
	return map[string]map[string]catalog.ModelSpec{
		"together": {"llama-4-70b": {
			Name: "Llama 4 70B", Source: "together", ContextWindow: 131_072, MaxTokens: 8_192,
		}},
		"groq": {"llama-4-70b": {
			Name: "Llama 4 70B", Source: "groq", ContextWindow: 8_192, MaxTokens: 8_192,
		}},
		"fireworks-ai": {
			"llama-4-70b": {
				Name: "Llama 4 70B", Source: "fireworks-ai", ContextWindow: 262_144, MaxTokens: 16_384,
			},
			"fireworks-only": {
				Name: "Fireworks Only", Source: "fireworks-ai", ContextWindow: 32_000, MaxTokens: 4_096,
			},
		},
	}
}

func captureWarnings(t *testing.T) *observer.ObservedLogs {
	t.Helper()

	core, logs := observer.New(zapcore.WarnLevel)
	prev := logger.L
	logger.L = zap.New(core)

	t.Cleanup(func() { logger.L = prev })

	return logs
}

// A bare openai provider silently borrows a different host's metadata for a
// duplicated id. The resolution stays as it is; the operator must be told.
func TestEnrichCatalogWarnsOnAmbiguousCatalogSection(t *testing.T) {
	logs := captureWarnings(t)

	driver := &stubDriver{models: catalog.Flatten(ambiguousSections())}

	cfg := testConfig([]config.ModelEntry{
		{ID: "llama-4-70b", Provider: "prod"},
		{ID: "fireworks-only", Provider: "prod"},
	})
	cfg.UnifiedConfig.Providers["prod"] = config.ProviderEntry{Driver: driverOpenAI}

	require.NoError(t, enrichCatalog(context.Background(), cfg, map[string]driverProtocol{driverOpenAI: driver}))

	warns := logs.FilterMessage("catalog_section_ambiguous").All()
	require.Len(t, warns, 1, "one warning per ambiguous id, none for unique ids")

	fields := warns[0].ContextMap()
	assert.Equal(t, "llama-4-70b", fields["id"])
	assert.Equal(t, "fireworks-ai", fields["resolved_from"])
	assert.Equal(t, []any{"groq", "together"}, fields["also_in"])
	assert.Equal(t, "prod", fields["provider"])
	assert.Contains(t, fields["hint"], "catalog:")

	// The warning is advice, not a behavior change.
	assert.Equal(t, 262_144, cfg.UnifiedConfig.Models[0].ContextWindow)
	assert.Equal(t, 32_000, cfg.UnifiedConfig.Models[1].ContextWindow)
}

// A provider pinned to one section resolves against that section alone, so there
// is nothing ambiguous to report.
func TestEnrichCatalogSilentForPinnedSection(t *testing.T) {
	logs := captureWarnings(t)

	driver := &stubDriver{models: ambiguousSections()["groq"]}

	cfg := testConfig([]config.ModelEntry{{ID: "llama-4-70b", Provider: "prod"}})
	cfg.UnifiedConfig.Providers["prod"] = config.ProviderEntry{Driver: driverOpenAI, Catalog: "groq"}

	require.NoError(t, enrichCatalog(context.Background(), cfg, map[string]driverProtocol{driverOpenAI: driver}))
	assert.Zero(t, logs.FilterMessage("catalog_section_ambiguous").Len())
	assert.Equal(t, 8_192, cfg.UnifiedConfig.Models[0].ContextWindow)
}

// The date-stripping fallback must carry the ambiguity too — a dated config id
// resolving onto a shadowed catalog key is the same trap.
func TestEnrichCatalogWarnsOnDateNormalizedAmbiguousMatch(t *testing.T) {
	logs := captureWarnings(t)

	driver := &stubDriver{models: catalog.Flatten(ambiguousSections())}

	cfg := testConfig([]config.ModelEntry{{ID: "llama-4-70b-20260101", Provider: "prod"}})
	cfg.UnifiedConfig.Providers["prod"] = config.ProviderEntry{Driver: driverOpenAI}

	require.NoError(t, enrichCatalog(context.Background(), cfg, map[string]driverProtocol{driverOpenAI: driver}))

	warns := logs.FilterMessage("catalog_section_ambiguous").All()
	require.Len(t, warns, 1)
	assert.Equal(t, "llama-4-70b-20260101", warns[0].ContextMap()["id"])
	assert.Equal(t, "fireworks-ai", warns[0].ContextMap()["resolved_from"])
}
