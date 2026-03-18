package llm

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
)

func TestNewClient_AnthropicDriver(t *testing.T) {
	cfg := &config.Config{
		Model: "claude-opus-4-6",
		UnifiedConfig: &config.UnifiedConfig{
			Providers: map[string]config.ProviderEntry{
				"anthropic": {Driver: "anthropic", APIKey: "sk-ant-test"}, //nolint:gosec // fake fixture, not real
			},
			Models: []config.ModelEntry{
				{ID: "claude-opus-4-6", Provider: "anthropic", MaxTokens: 32000, ContextWindow: 200000},
			},
		},
	}

	client, err := NewClient(cfg)
	require.NoError(t, err)
	defer client.Close()

	// RetryableClient wraps the inner client
	assert.Equal(t, "claude-opus-4-6", client.Model())
}

func TestNewClient_OpenAIDriver(t *testing.T) {
	cfg := &config.Config{
		Model: "minimax/minimax-m2.5",
		UnifiedConfig: &config.UnifiedConfig{
			Providers: map[string]config.ProviderEntry{
				"openrouter": {Driver: "openai", APIKey: "sk-or-test", BaseURL: "https://openrouter.ai/api/v1"},
			},
			Models: []config.ModelEntry{
				{ID: "minimax/minimax-m2.5", Provider: "openrouter", ContextWindow: 1000000},
			},
		},
	}

	client, err := NewClient(cfg)
	require.NoError(t, err)
	defer client.Close()

	assert.Equal(t, "minimax/minimax-m2.5", client.Model())
}

func TestNewClient_ModelNotFound(t *testing.T) {
	cfg := &config.Config{
		Model: "nonexistent-model",
		UnifiedConfig: &config.UnifiedConfig{
			Providers: map[string]config.ProviderEntry{
				"openrouter": {Driver: "openai", APIKey: "sk-test"},
			},
			Models: []config.ModelEntry{
				{ID: "some-model", Provider: "openrouter"},
			},
		},
	}

	_, err := NewClient(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found in config")
}

func TestNewClient_NoModel(t *testing.T) {
	cfg := &config.Config{
		UnifiedConfig: &config.UnifiedConfig{},
	}

	_, err := NewClient(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no model configured")
}

func TestNewClient_DefaultsToFirstModel(t *testing.T) {
	cfg := &config.Config{
		UnifiedConfig: &config.UnifiedConfig{
			Providers: map[string]config.ProviderEntry{
				"openrouter": {Driver: "openai", APIKey: "sk-test", BaseURL: "https://openrouter.ai/api/v1"},
			},
			Models: []config.ModelEntry{
				{ID: "first-model", Provider: "openrouter"},
			},
		},
	}

	client, err := NewClient(cfg)
	require.NoError(t, err)
	defer client.Close()

	assert.Equal(t, "first-model", client.Model())
}

func TestNewClientWithModel_EmptyFallsBackToNewClient(t *testing.T) {
	cfg := &config.Config{
		Model: "main-model",
		UnifiedConfig: &config.UnifiedConfig{
			Providers: map[string]config.ProviderEntry{
				"openrouter": {Driver: "openai", APIKey: "sk-test", BaseURL: "https://openrouter.ai/api/v1"},
			},
			Models: []config.ModelEntry{
				{ID: "main-model", Provider: "openrouter"},
			},
		},
	}

	client, err := NewClientWithModel(cfg, "")
	require.NoError(t, err)
	defer client.Close()

	assert.Equal(t, "main-model", client.Model())
}

func TestNewClient_OpenRouterDriver(t *testing.T) {
	cfg := &config.Config{
		Model: "openai/gpt-4o",
		UnifiedConfig: &config.UnifiedConfig{
			Providers: map[string]config.ProviderEntry{
				"or": {Driver: "openrouter", APIKey: "sk-or-test", BaseURL: "https://openrouter.ai/api/v1"},
			},
			Models: []config.ModelEntry{
				{ID: "openai/gpt-4o", Provider: "or", ContextWindow: 128000},
			},
		},
	}

	client, err := NewClient(cfg)
	require.NoError(t, err)
	defer client.Close()

	assert.Equal(t, "openai/gpt-4o", client.Model())
	assert.Equal(t, "openrouter", client.Provider())
}
