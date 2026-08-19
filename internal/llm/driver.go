package llm

import (
	"context"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/catalog"
	"github.com/pilat/coagent/internal/config"
)

const (
	driverAnthropic  = "anthropic"
	driverOpenAI     = "openai"
	driverOpenRouter = "openrouter"
	driverGoogleSA   = "google-sa"

	sectionAnthropic    = "anthropic"
	sectionGoogleVertex = "google-vertex"
)

// defaultDrivers is the process-wide registry; its shared Fetcher memoizes
// models.dev so every driver pays for that catalog exactly once.
var defaultDrivers = newDrivers(catalog.New())

// driverProtocol owns one provider protocol end to end. Both methods are mandatory, so
// the compiler will not let a new driver skip saying where its models come from.
type driverProtocol interface {
	NewClient(entry config.ProviderEntry, model config.ModelEntry) (Client, error)
	ListModels(
		ctx context.Context,
		providerKey string,
		entry config.ProviderEntry,
	) (map[string]catalog.ModelSpec, error)
}

var (
	_ driverProtocol = (*anthropicDriver)(nil)
	_ driverProtocol = (*openAIDriver)(nil)
	_ driverProtocol = (*googleSADriver)(nil)
	_ driverProtocol = (*openRouterDriver)(nil)
)

type (
	anthropicDriver  struct{ catalog catalog.Fetcher }
	openAIDriver     struct{ catalog catalog.Fetcher }
	googleSADriver   struct{ catalog catalog.Fetcher }
	openRouterDriver struct{ catalog catalog.Fetcher }
)

func newDrivers(f catalog.Fetcher) map[string]driverProtocol {
	return map[string]driverProtocol{
		driverAnthropic:  &anthropicDriver{catalog: f},
		driverOpenAI:     &openAIDriver{catalog: f},
		driverGoogleSA:   &googleSADriver{catalog: f},
		driverOpenRouter: &openRouterDriver{catalog: f},
	}
}

func (d *anthropicDriver) NewClient(entry config.ProviderEntry, model config.ModelEntry) (Client, error) {
	return newAnthropicClient(anthropicParams{APIKey: entry.APIKey, Model: model})
}

func (d *anthropicDriver) ListModels(
	ctx context.Context,
	_ string,
	entry config.ProviderEntry,
) (map[string]catalog.ModelSpec, error) {
	return modelsDevSectionFor(ctx, d.catalog, entry, sectionAnthropic)
}

func (d *openAIDriver) NewClient(entry config.ProviderEntry, model config.ModelEntry) (Client, error) {
	return newOpenAICompatibleClient(openAICompatibleParams{
		BaseURL: entry.BaseURL,
		APIKey:  entry.APIKey,
		Model:   model,
	})
}

// ListModels searches every models.dev section when no catalog key pins one —
// "openai" as a driver name says nothing about which vendor is behind the endpoint.
func (d *openAIDriver) ListModels(
	ctx context.Context,
	_ string,
	entry config.ProviderEntry,
) (map[string]catalog.ModelSpec, error) {
	sections, err := fetchModelsDev(ctx, d.catalog)
	if err != nil {
		return nil, err
	}

	if entry.Catalog == "" {
		return catalog.Flatten(sections), nil
	}

	return sectionOrError(sections, entry.Catalog)
}

func (d *googleSADriver) NewClient(entry config.ProviderEntry, model config.ModelEntry) (Client, error) {
	if entry.BaseURL == "" {
		return nil, errors.New("google-sa driver requires base_url")
	}

	if model.ID == "" {
		return nil, errors.New("google-sa driver requires a model")
	}

	ts, err := newGoogleTokenSource(entry.SAFile)
	if err != nil {
		return nil, err
	}

	return newOpenAICompatibleClient(openAICompatibleParams{
		BaseURL:     entry.BaseURL,
		Model:       model,
		TokenSource: ts,
	})
}

func (d *googleSADriver) ListModels(
	ctx context.Context,
	_ string,
	entry config.ProviderEntry,
) (map[string]catalog.ModelSpec, error) {
	return modelsDevSectionFor(ctx, d.catalog, entry, sectionGoogleVertex)
}

func (d *openRouterDriver) NewClient(entry config.ProviderEntry, model config.ModelEntry) (Client, error) {
	return newOpenAICompatibleClient(openAICompatibleParams{
		BaseURL:      entry.BaseURL,
		APIKey:       entry.APIKey,
		Model:        model,
		IsOpenRouter: true,
	})
}

// ListModels ignores the catalog key: OpenRouter serves its own first-party list.
func (d *openRouterDriver) ListModels(
	ctx context.Context,
	_ string,
	entry config.ProviderEntry,
) (map[string]catalog.ModelSpec, error) {
	body, err := d.catalog.Fetch(ctx, openRouterSource(entry.BaseURL))
	if err != nil {
		return nil, fmt.Errorf("openrouter models: %w", err)
	}

	models, err := parseOpenRouter(body)
	if err != nil {
		return nil, fmt.Errorf("openrouter models: %w", err)
	}

	return models, nil
}

// CatalogSection names the catalog a provider's models resolve against: the
// entry's explicit choice, or the driver's own default.
//
// A bare `openai` provider has none, and reports false. The driver name says
// nothing about the vendor behind the endpoint, so callers must supply a model
// id rather than guess one.
func CatalogSection(entry config.ProviderEntry) (string, bool) {
	if entry.Catalog != "" {
		return entry.Catalog, true
	}

	switch entry.Driver {
	case driverAnthropic:
		return sectionAnthropic, true
	case driverGoogleSA:
		return sectionGoogleVertex, true
	case driverOpenRouter:
		return driverOpenRouter, true
	default:
		return "", false
	}
}

// fetchModelsDev retrieves and parses the shared community catalog. The fetcher
// memoizes by URL, so the three drivers reading it pay for one request.
func fetchModelsDev(ctx context.Context, f catalog.Fetcher) (map[string]map[string]catalog.ModelSpec, error) {
	body, err := f.Fetch(ctx, modelsDevSource)
	if err != nil {
		return nil, fmt.Errorf("models.dev: %w", err)
	}

	sections, err := parseModelsDev(body)
	if err != nil {
		return nil, fmt.Errorf("models.dev: %w", err)
	}

	return sections, nil
}

func modelsDevSectionFor(
	ctx context.Context,
	f catalog.Fetcher,
	entry config.ProviderEntry,
	fallback string,
) (map[string]catalog.ModelSpec, error) {
	sections, err := fetchModelsDev(ctx, f)
	if err != nil {
		return nil, err
	}

	name := entry.Catalog
	if name == "" {
		name = fallback
	}

	return sectionOrError(sections, name)
}

func sectionOrError(
	sections map[string]map[string]catalog.ModelSpec,
	name string,
) (map[string]catalog.ModelSpec, error) {
	models, ok := sections[name]
	if !ok {
		return nil, fmt.Errorf("models.dev has no section %q", name)
	}

	return models, nil
}
