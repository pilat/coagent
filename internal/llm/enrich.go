package llm

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/catalog"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/logger"
)

// anthropicModelPrefix marks OpenAI-compatible ids routed to an Anthropic
// backend, which mandates max_tokens whatever the configured driver is.
const anthropicModelPrefix = "anthropic/"

// budgetEffortLevels is what we offer when the catalog names no levels: the three
// the budget mapping covers, and a safe subset of every gateway vocabulary.
var budgetEffortLevels = []string{
	string(ReasoningLow),
	string(ReasoningMedium),
	string(ReasoningHigh),
}

// EnrichCatalog fills every configured model from its driver's catalog. A model the
// catalog lost keeps its last resolution; one that never resolved fails startup.
func EnrichCatalog(ctx context.Context, cfg *config.Config) error {
	return enrichWithResolutions(ctx, cfg, defaultDrivers, catalog.LoadResolutions(ctx))
}

func enrichWithResolutions(
	ctx context.Context, cfg *config.Config, drivers map[string]driverProtocol, memory *catalog.Resolutions,
) error {
	uc := cfg.UnifiedConfig
	if uc == nil || len(uc.Models) == 0 {
		return nil
	}

	byProvider := make(map[string][]int, len(uc.Providers))
	for i, m := range uc.Models {
		byProvider[m.Provider] = append(byProvider[m.Provider], i)
	}

	var problems []string

	resolved := make([]bool, len(uc.Models))

	for _, providerKey := range slices.Sorted(maps.Keys(byProvider)) {
		unresolved, err := enrichProvider(ctx, uc, drivers, providerKey, byProvider[providerKey], resolved, memory)
		if err != nil {
			return err
		}

		problems = append(problems, unresolved...)
	}

	problems = append(problems, validateEnriched(uc, resolved)...)
	if len(problems) > 0 {
		return fmt.Errorf("model catalog resolution failed:\n  - %s", strings.Join(problems, "\n  - "))
	}

	memory.Save(ctx)
	logEnriched(uc)

	return nil
}

// enrichProvider fills every model of one provider from a single ListModels call,
// returning the ids neither the catalog nor a last resolution knew.
func enrichProvider(
	ctx context.Context,
	uc *config.UnifiedConfig,
	drivers map[string]driverProtocol,
	providerKey string,
	indices []int,
	resolved []bool,
	memory *catalog.Resolutions,
) ([]string, error) {
	entry, ok := uc.Providers[providerKey]
	if !ok {
		return nil, fmt.Errorf("models reference unknown provider %q", providerKey)
	}

	driver, ok := drivers[entry.Driver]
	if !ok {
		return nil, fmt.Errorf("provider %q has unsupported driver %q", providerKey, entry.Driver)
	}

	// A dead catalog is reported, not returned: startup fails either way, and the
	// operator should see every broken provider in one run rather than one per fix.
	models, err := driver.ListModels(ctx, providerKey, entry)

	sectionMissing := errors.Is(err, errSectionMissing)
	if err != nil && !sectionMissing {
		return []string{catalogUnavailable(providerKey, err)}, nil
	}

	var problems []string

	for _, idx := range indices {
		problem := resolveModel(ctx, &uc.Models[idx], providerKey, entry, models, memory)
		if problem != "" {
			problems = append(problems, problem)

			continue
		}

		resolved[idx] = true
	}

	// Preserve today's provider error for a typo'd section or a model that never
	// resolved; both must keep the missing-section diagnosis.
	if sectionMissing && len(problems) > 0 {
		return []string{catalogUnavailable(providerKey, err)}, nil
	}

	return problems, nil
}

func catalogUnavailable(providerKey string, err error) string {
	return fmt.Sprintf("provider %q: catalog unavailable: %v", providerKey, err)
}

// resolveModel fills one model from the catalog, else from its last resolution,
// and returns the problem when neither knows it.
func resolveModel(
	ctx context.Context,
	m *config.ModelEntry,
	providerKey string,
	entry config.ProviderEntry,
	models map[string]catalog.ModelSpec,
	memory *catalog.Resolutions,
) string {
	spec, found := catalog.Lookup(models, m.ID)
	if found {
		warnAmbiguousSection(providerKey, m.ID, spec)
		applySpec(m, providerKey, entry, spec)
		memory.Remember(m.ID, entry, spec)

		return ""
	}

	last, known := memory.Recall(m.ID, entry)
	if !known {
		return fmt.Sprintf("%s (provider %q): not present in its catalog", m.ID, providerKey)
	}

	logger.Ctx(ctx).Named("llm.catalog").Warn("catalog_model_missing_upstream",
		zap.String("id", m.ID),
		zap.String("provider", providerKey),
		zap.Duration("age", time.Since(last.ResolvedAt)))

	applySpec(m, providerKey, entry, last.Spec)
	m.CatalogStale = true

	return ""
}

// warnAmbiguousSection reports a duplicated id a bare provider's flatten resolved
// by section name — the window and prices may be a host the operator is not calling.
func warnAmbiguousSection(providerKey, id string, spec catalog.ModelSpec) {
	if len(spec.Shadowed) == 0 {
		return
	}

	logger.Named("llm.catalog").Warn("catalog_section_ambiguous",
		zap.String("provider", providerKey),
		zap.String("id", id),
		zap.String("resolved_from", spec.Source),
		zap.Strings("also_in", spec.Shadowed),
		zap.String("hint", "set catalog: <section> on the provider to pin the metadata"))
}

func applySpec(m *config.ModelEntry, providerKey string, entry config.ProviderEntry, spec catalog.ModelSpec) {
	m.Name = spec.Name
	m.DisplayName = providerKey + "/" + spec.Name

	if spec.Name == "" {
		m.Name = m.ID
		m.DisplayName = m.ID
	}

	m.ContextWindow = spec.ContextWindow
	m.MaxTokens = spec.MaxTokens
	m.Pricing = spec.Pricing
	m.Reasoning = spec.Reasoning
	m.InputModalities = spec.InputModalities
	m.EffortLevels = effortLevels(entry, spec.Reasoning)
	m.DefaultEffort = defaultEffort(spec.Reasoning, m.EffortLevels)
}

// effortLevels narrows a catalog's reasoning spec to the levels the picker may
// offer. A driver that never puts the level on the wire offers none, and neither
// does a model whose catalog declares no effort selector.
func effortLevels(entry config.ProviderEntry, spec *config.ReasoningSpec) []string {
	if spec == nil || !spec.Supported {
		return nil
	}

	switch entry.Driver {
	case driverAnthropic:
		if spec.NativeEffort && len(spec.Efforts) > 0 {
			return spec.Efforts
		}

		// Budget-token models take no level, but the driver derives the budget
		// from one, so the choice is real even though the catalog lists none.
		return budgetEffortLevels
	case driverOpenRouter:
		if spec.AnyEffort {
			return budgetEffortLevels
		}

		return spec.Efforts
	case driverOpenAI:
		// Only the Responses client puts a level on the wire.
		if isDirectOpenAI(entry.BaseURL) && len(spec.Efforts) > 0 {
			return spec.Efforts
		}
	}

	return nil
}

// defaultEffort picks what a switch to this model lands on: the catalog's own
// preference when it has one, medium when it is on offer, else the middle level.
func defaultEffort(spec *config.ReasoningSpec, levels []string) string {
	if len(levels) == 0 {
		return ""
	}

	// "none" as a catalog default means reasoning-off, not a level to pre-select.
	if spec != nil && spec.Default != string(ReasoningNone) && slices.Contains(levels, spec.Default) {
		return spec.Default
	}

	if slices.Contains(levels, string(ReasoningMedium)) {
		return string(ReasoningMedium)
	}

	return levels[len(levels)/2]
}

// validateEnriched mirrors what the client constructors actually demand, not the
// driver list: max_tokens is required wherever an Anthropic backend serves.
func validateEnriched(uc *config.UnifiedConfig, resolved []bool) []string {
	var problems []string

	for i, m := range uc.Models {
		if !resolved[i] {
			continue
		}

		if m.ContextWindow <= 0 {
			problems = append(problems, m.ID+": catalog carries no context window")
		}

		if requiresMaxTokens(uc, m) && m.MaxTokens <= 0 {
			problems = append(problems, m.ID+": catalog carries no max output tokens")
		}
	}

	if problem := browserEffortProblem(uc, resolved); problem != "" {
		problems = append(problems, problem)
	}

	return problems
}

// browserEffortProblem refuses a pinned browser effort the model cannot take,
// never clamping it: the operator's level must be the one that runs.
func browserEffortProblem(uc *config.UnifiedConfig, resolved []bool) string {
	b := uc.Browser
	if b == nil || b.Effort == "" {
		return ""
	}

	i := slices.IndexFunc(uc.Models, func(m config.ModelEntry) bool { return m.ID == b.Model })
	if i < 0 || !resolved[i] {
		return ""
	}

	levels := uc.Models[i].EffortLevels
	if len(levels) == 0 {
		return fmt.Sprintf("browser.effort %q: model %s accepts no effort level", b.Effort, b.Model)
	}

	if !slices.Contains(levels, b.Effort) {
		return fmt.Sprintf("browser.effort %q: model %s accepts only %s", b.Effort, b.Model, strings.Join(levels, ", "))
	}

	return ""
}

func requiresMaxTokens(uc *config.UnifiedConfig, m config.ModelEntry) bool {
	if uc.Providers[m.Provider].Driver == driverAnthropic {
		return true
	}

	return strings.HasPrefix(m.ID, anthropicModelPrefix)
}

func logEnriched(uc *config.UnifiedConfig) {
	log := logger.Named("llm.catalog")

	for _, m := range uc.Models {
		reasoning := m.Reasoning != nil && m.Reasoning.Supported

		log.Info("model_enriched",
			zap.String("id", m.ID),
			zap.String("display_name", m.DisplayName),
			zap.Int("context_window", m.ContextWindow),
			zap.Int("max_tokens", m.MaxTokens),
			zap.Bool("reasoning", reasoning),
			zap.Strings("efforts", m.EffortLevels),
			zap.String("default_effort", m.DefaultEffort))
	}
}
