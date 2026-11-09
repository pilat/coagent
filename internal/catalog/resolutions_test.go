package catalog

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/logger"
)

func TestResolutionsRoundTripEveryField(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	provider := config.ProviderEntry{Driver: "openrouter"}
	spec := ModelSpec{
		Name: "Model", Source: "catalog", ContextWindow: 100000, MaxTokens: 4000,
		Pricing: &config.ModelPricing{InputPrice: 1, OutputPrice: 2, CacheReadPrice: 3, CacheWritePrice: 4},
		Reasoning: &config.ReasoningSpec{
			Supported: true, NativeEffort: true, BudgetMin: 100,
			Efforts: []string{"low", "high"}, AnyEffort: true, Default: "high",
		},
		InputModalities: []string{"text", "image"}, Shadowed: []string{"other"},
	}
	assertResolutionFieldsNonzero(t, reflect.ValueOf(spec))
	memory := LoadResolutions(ctx)
	memory.Remember("full", provider, spec)
	memory.Remember("nil", provider, ModelSpec{})
	memory.Remember("empty", provider, ModelSpec{InputModalities: []string{}, Reasoning: &config.ReasoningSpec{}})
	want, ok := memory.Recall("full", provider)
	require.True(t, ok)
	memory.Save(ctx)
	loaded := LoadResolutions(ctx)
	got, ok := loaded.Recall("full", provider)
	require.True(t, ok)
	assert.Equal(t, want.Spec, got.Spec)
	assert.True(t, want.ResolvedAt.Equal(got.ResolvedAt))
	nilSpec, ok := loaded.Recall("nil", provider)
	require.True(t, ok)
	assert.Nil(t, nilSpec.Spec.Pricing)
	assert.Nil(t, nilSpec.Spec.Reasoning)
	assert.Nil(t, nilSpec.Spec.InputModalities)
	empty, ok := loaded.Recall("empty", provider)
	require.True(t, ok)
	assert.Equal(t, []string{}, empty.Spec.InputModalities)
	assert.Nil(t, empty.Spec.Reasoning.Efforts)
}

func TestResolutionsMissingFileIsEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, logs := resolutionWarnings()
	_, ok := LoadResolutions(ctx).Recall("missing", config.ProviderEntry{})
	assert.False(t, ok)
	assert.Zero(t, logs.Len())
}

func TestResolutionsCorruptFileIsAbsent(t *testing.T) {
	for _, body := range []string{`{"model":`, `<html>`, `[]`} {
		t.Run(body, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			ctx, logs := resolutionWarnings()
			dir := defaultCacheDir()
			require.NoError(t, os.MkdirAll(dir, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(dir, resolutionsFileName), []byte(body), 0o600))
			memory := LoadResolutions(ctx)
			_, ok := memory.Recall("model", config.ProviderEntry{})
			assert.False(t, ok)
			assert.Equal(t, 1, logs.FilterMessage("catalog_resolutions_read_failed").Len())
			memory.Remember("model", config.ProviderEntry{}, ModelSpec{Name: "repaired"})
			memory.Save(ctx)
			got, ok := LoadResolutions(ctx).Recall("model", config.ProviderEntry{})
			require.True(t, ok)
			assert.Equal(t, "repaired", got.Spec.Name)
			assert.Equal(t, 1, logs.Len())
		})
	}
}

func TestResolutionsRecallRefusesAnotherOrigin(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	provider := config.ProviderEntry{
		Driver:  "openrouter",
		Catalog: "one",
		BaseURL: "http://example.test",
		APIKey:  "old",
	}
	memory := LoadResolutions(context.Background())
	memory.Remember("model", provider, ModelSpec{Name: "x"})
	for _, tt := range []struct {
		name   string
		change func(*config.ProviderEntry)
		known  bool
	}{
		{"driver", func(p *config.ProviderEntry) { p.Driver = "openai" }, false},
		{"catalog", func(p *config.ProviderEntry) { p.Catalog = "two" }, false},
		{"base_url", func(p *config.ProviderEntry) { p.BaseURL += "/new" }, false},
		{"api_key", func(p *config.ProviderEntry) { p.APIKey = "new" }, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			changed := provider
			tt.change(&changed)
			_, ok := memory.Recall("model", changed)
			assert.Equal(t, tt.known, ok)
		})
	}
}

func TestResolutionsSaveKeepsEntriesItDidNotTouch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	provider := config.ProviderEntry{}
	memory := LoadResolutions(ctx)
	memory.Remember("a", provider, ModelSpec{Name: "A"})
	memory.Remember("b", provider, ModelSpec{Name: "B"})
	want, _ := memory.Recall("b", provider)
	memory.Save(ctx)
	memory = LoadResolutions(ctx)
	memory.Remember("a", provider, ModelSpec{Name: "new A"})
	memory.Save(ctx)
	got, ok := LoadResolutions(ctx).Recall("b", provider)
	require.True(t, ok)
	assert.Equal(t, want.Spec, got.Spec)
	assert.True(t, want.ResolvedAt.Equal(got.ResolvedAt))
}

func TestResolutionsKeepsEachOriginsLastResolution(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	a := config.ProviderEntry{Driver: "openrouter", BaseURL: "http://a.test"}
	b := config.ProviderEntry{Driver: "openrouter", BaseURL: "http://b.test"}
	memory := LoadResolutions(ctx)
	memory.Remember("model", a, ModelSpec{Name: "Model A", ContextWindow: 1000})
	first, ok := memory.Recall("model", a)
	require.True(t, ok)
	memory.Save(ctx)
	memory = LoadResolutions(ctx)
	memory.Remember("model", b, ModelSpec{Name: "Model B", ContextWindow: 2000})
	second, ok := memory.Recall("model", b)
	require.True(t, ok)
	require.True(t, second.ResolvedAt.After(first.ResolvedAt))
	memory.Save(ctx)
	memory = LoadResolutions(ctx)
	recalledA, ok := memory.Recall("model", a)
	require.True(t, ok)
	assert.Equal(t, first.Spec, recalledA.Spec)
	assert.True(t, first.ResolvedAt.Equal(recalledA.ResolvedAt))
	recalledB, ok := memory.Recall("model", b)
	require.True(t, ok)
	assert.Equal(t, second.Spec, recalledB.Spec)
	assert.True(t, second.ResolvedAt.Equal(recalledB.ResolvedAt))
	_, ok = memory.Recall("model", config.ProviderEntry{Driver: "openrouter", BaseURL: "http://unknown.test"})
	assert.False(t, ok)
}

func TestResolutionsFileCarriesNoCredentials(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	endpoint := url.URL{Scheme: "http", Host: "example.test", User: url.UserPassword("user", "hunter2")}
	provider := config.ProviderEntry{BaseURL: endpoint.String(), APIKey: "private-api-key"}
	memory := LoadResolutions(ctx)
	memory.Remember("model", provider, ModelSpec{Name: "x"})
	memory.Save(ctx)
	body, err := os.ReadFile(filepath.Join(defaultCacheDir(), resolutionsFileName))
	require.NoError(t, err)
	assert.NotContains(t, string(body), "user:hunter2@")
	assert.NotContains(t, string(body), "hunter2")
	assert.NotContains(t, string(body), provider.APIKey)
	assert.NotContains(t, string(body), provider.BaseURL)
}

func TestResolutionsZeroValueHasNoFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	memory := &Resolutions{}
	provider := config.ProviderEntry{}
	_, ok := memory.Recall("model", provider)
	assert.False(t, ok)
	memory.Remember("model", provider, ModelSpec{Name: "x"})
	_, ok = memory.Recall("model", provider)
	assert.True(t, ok)
	assert.NotPanics(t, func() { memory.Save(context.Background()) })
	_, err := os.Stat(defaultCacheDir())
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestResolutionsSaveFailureOnlyWarns(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, logs := resolutionWarnings()
	path := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(path, []byte("file"), 0o600))
	memory := loadResolutionsFrom(ctx, path)
	memory.Remember("model", config.ProviderEntry{}, ModelSpec{Name: "x"})
	assert.NotPanics(t, func() { memory.Save(ctx) })
	assert.Equal(t, 1, logs.FilterMessage("catalog_resolutions_write_failed").Len())
	assert.Equal(t, 1, logs.FilterMessage("catalog_resolutions_read_failed").Len())
}

func TestResolutionsSaveLeavesOnlyTheFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	memory := LoadResolutions(ctx)
	memory.Remember("model", config.ProviderEntry{}, ModelSpec{Name: "x"})
	memory.Save(ctx)
	entries, err := os.ReadDir(defaultCacheDir())
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, resolutionsFileName, entries[0].Name())
	info, err := entries[0].Info()
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func resolutionWarnings() (context.Context, *observer.ObservedLogs) {
	core, logs := observer.New(zap.WarnLevel)

	return logger.ToContext(context.Background(), zap.New(core)), logs
}

func assertResolutionFieldsNonzero(t *testing.T, value reflect.Value) {
	t.Helper()
	if value.Kind() == reflect.Pointer {
		require.False(t, value.IsNil())
		assertResolutionFieldsNonzero(t, value.Elem())

		return
	}
	if value.Kind() != reflect.Struct {
		require.False(t, value.IsZero(), "zero fixture value of %s", value.Type())

		return
	}
	for i := range value.NumField() {
		t.Run(value.Type().Field(i).Name, func(t *testing.T) {
			assertResolutionFieldsNonzero(t, value.Field(i))
		})
	}
}
