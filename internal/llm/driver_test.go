package llm

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/catalog"
	"github.com/pilat/coagent/internal/config"
)

var _ catalog.Fetcher = (*fakeFetcher)(nil)

// The transport hands back raw bodies, so the fake serves JSON per URL — which is
// also what proves each driver asks for its own endpoint.
const (
	fakeModelsDevBody = `{
	  "anthropic":     {"models": {"claude-opus-5": {"name": "Claude Opus 5",
	                    "limit": {"context": 1000000, "output": 128000}}}},
	  "google-vertex": {"models": {"claude-opus-4-5@20251101": {"name": "Claude Opus 4.5",
	                    "limit": {"context": 200000, "output": 64000}}}},
	  "deepseek":      {"models": {"shared": {"name": "From DeepSeek", "limit": {"context": 32768}}}},
	  "zai":           {"models": {"shared": {"name": "From ZAI", "limit": {"context": 16384}},
	                    "glm-5":  {"name": "GLM 5", "limit": {"context": 200000}}}}
	}`

	fakeOpenRouterBody = `{"data": [{"id": "anthropic/claude-opus-5", "name": "Anthropic: Claude Opus 5",
	  "context_length": 1000000, "pricing": {"prompt": "0", "completion": "0"},
	  "top_provider": {"context_length": 900000, "max_completion_tokens": 128000}}]}`
)

type fakeFetcher struct {
	bodies  map[string]string
	lastURL string
	err     error
}

func (f *fakeFetcher) Fetch(_ context.Context, src catalog.Source) ([]byte, error) {
	f.lastURL = src.URL

	if f.err != nil {
		return nil, f.err
	}

	body, ok := f.bodies[src.URL]
	if !ok {
		return nil, errors.New("no fake body for " + src.URL)
	}

	return []byte(body), nil
}

func testFetcher() *fakeFetcher {
	return &fakeFetcher{bodies: map[string]string{
		modelsDevURL:                    fakeModelsDevBody,
		defaultOpenRouterURL:            fakeOpenRouterBody,
		"https://proxy.local/v1/models": fakeOpenRouterBody,
	}}
}

func TestDriverListModels(t *testing.T) {
	tests := []struct {
		name       string
		driverName string
		entry      config.ProviderEntry
		wantModel  string
		wantSource string
	}{
		{
			name:       "anthropic defaults to its own section",
			driverName: driverAnthropic,
			entry:      config.ProviderEntry{Driver: driverAnthropic},
			wantModel:  "claude-opus-5",
			wantSource: "anthropic",
		},
		{
			name:       "google-sa defaults to google-vertex",
			driverName: driverGoogleSA,
			entry:      config.ProviderEntry{Driver: driverGoogleSA},
			wantModel:  "claude-opus-4-5@20251101",
			wantSource: "google-vertex",
		},
		{
			name:       "explicit catalog key overrides the default section",
			driverName: driverAnthropic,
			entry:      config.ProviderEntry{Driver: driverAnthropic, Catalog: "zai"},
			wantModel:  "glm-5",
			wantSource: "zai",
		},
		{
			name:       "openai with a catalog key uses that section",
			driverName: driverOpenAI,
			entry:      config.ProviderEntry{Driver: driverOpenAI, Catalog: "zai"},
			wantModel:  "glm-5",
			wantSource: "zai",
		},
		{
			name:       "openai without a catalog key searches every section in sorted order",
			driverName: driverOpenAI,
			entry:      config.ProviderEntry{Driver: driverOpenAI},
			wantModel:  "shared",
			wantSource: "deepseek",
		},
		{
			name:       "openrouter uses its first-party list",
			driverName: driverOpenRouter,
			entry:      config.ProviderEntry{Driver: driverOpenRouter, BaseURL: "https://openrouter.ai/api/v1"},
			wantModel:  "anthropic/claude-opus-5",
			wantSource: "openrouter",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			drivers := newDrivers(testFetcher())

			models, err := drivers[tt.driverName].ListModels(context.Background(), "p", tt.entry)
			require.NoError(t, err)

			spec, ok := models[tt.wantModel]
			require.True(t, ok, "expected %q in the driver's model set", tt.wantModel)
			assert.Equal(t, tt.wantSource, spec.Source)
		})
	}
}

func TestDriverListModelsUnknownSection(t *testing.T) {
	drivers := newDrivers(testFetcher())

	_, err := drivers[driverAnthropic].ListModels(
		context.Background(), "p", config.ProviderEntry{Catalog: "nope"})
	assert.ErrorContains(t, err, "no section")
}

func TestDriverListModelsPropagatesFetchFailure(t *testing.T) {
	f := testFetcher()
	f.err = errors.New("offline")

	drivers := newDrivers(f)

	for _, name := range []string{driverAnthropic, driverOpenAI, driverGoogleSA, driverOpenRouter} {
		t.Run(name, func(t *testing.T) {
			_, err := drivers[name].ListModels(context.Background(), "p", config.ProviderEntry{})
			assert.ErrorContains(t, err, "offline")
		})
	}
}

func TestOpenRouterDriverDerivesURLFromBaseURL(t *testing.T) {
	f := testFetcher()
	drivers := newDrivers(f)

	_, err := drivers[driverOpenRouter].ListModels(
		context.Background(), "p", config.ProviderEntry{BaseURL: "https://proxy.local/v1"})
	require.NoError(t, err)
	assert.Equal(t, "https://proxy.local/v1/models", f.lastURL)
}

func TestDriverNewClient(t *testing.T) {
	drivers := newDrivers(testFetcher())

	tests := []struct {
		name         string
		driverName   string
		entry        config.ProviderEntry
		model        config.ModelEntry
		wantProvider string
	}{
		{
			name:         "anthropic",
			driverName:   driverAnthropic,
			entry:        config.ProviderEntry{APIKey: "key"},
			model:        config.ModelEntry{ID: "claude-opus-5", MaxTokens: 64000, ContextWindow: 1_000_000},
			wantProvider: "anthropic",
		},
		{
			name:         "openai-compatible",
			driverName:   driverOpenAI,
			entry:        config.ProviderEntry{APIKey: "key", BaseURL: "https://api.example.com/v1"},
			model:        config.ModelEntry{ID: "some-model", ContextWindow: 128_000},
			wantProvider: "openai-compatible",
		},
		{
			name:         "openrouter",
			driverName:   driverOpenRouter,
			entry:        config.ProviderEntry{APIKey: "key", BaseURL: "https://openrouter.ai/api/v1"},
			model:        config.ModelEntry{ID: "anthropic/claude-opus-5", MaxTokens: 64000, ContextWindow: 900_000},
			wantProvider: "openrouter",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, err := drivers[tt.driverName].NewClient(tt.entry, tt.model, DriverClientOpts{})
			require.NoError(t, err)

			assert.Equal(t, tt.wantProvider, client.Provider())
			assert.Equal(t, tt.model.ID, client.Model())
			assert.Equal(t, tt.model.ContextWindow, client.ContextWindow())
		})
	}
}

func TestDriverNewClientRejectsAnthropicWithoutMaxTokens(t *testing.T) {
	drivers := newDrivers(testFetcher())

	_, err := drivers[driverAnthropic].NewClient(
		config.ProviderEntry{APIKey: "key"}, config.ModelEntry{ID: "claude-opus-5"}, DriverClientOpts{})
	require.ErrorContains(t, err, "max_tokens")

	_, err = drivers[driverOpenRouter].NewClient(
		config.ProviderEntry{APIKey: "key", BaseURL: "https://openrouter.ai/api/v1"},
		config.ModelEntry{ID: "anthropic/claude-opus-5"},
		DriverClientOpts{},
	)
	require.ErrorContains(t, err, "max_tokens")
}

func TestDefaultDriversCoverEveryConfiguredDriver(t *testing.T) {
	for _, name := range []string{driverAnthropic, driverOpenAI, driverGoogleSA, driverOpenRouter} {
		assert.Contains(t, defaultDrivers, name)
	}
}
