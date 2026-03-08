package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

func TestUnifiedConfig_ValidConfig(t *testing.T) {
	path := writeConfig(t, `
providers:
  anthropic:
    driver: anthropic
    api_key: sk-ant-test
  openrouter:
    driver: openai
    api_key: sk-or-test
    base_url: https://openrouter.ai/api/v1

models:
  - id: claude-opus-4-6
    provider: anthropic
  - id: minimax/minimax-m2.5
    provider: openrouter
    timeout_sec: 900
    openrouter_config:
      only: [inceptron/fp8]
`)

	cfg, err := LoadUnifiedConfig(path, nil)
	require.NoError(t, err)
	require.Len(t, cfg.Providers, 2)
	require.Len(t, cfg.Models, 2)

	assert.Equal(t, "anthropic", cfg.Providers["anthropic"].Driver)
	assert.Equal(t, "openrouter", cfg.Models[1].Provider)
	assert.Equal(t, 900, cfg.Models[1].TimeoutSec)
	assert.Equal(t, []string{"inceptron/fp8"}, cfg.Models[1].OpenRouterConfig.Only)
}

func TestUnifiedConfig_ProviderCatalogKey(t *testing.T) {
	path := writeConfig(t, `
providers:
  vertex:
    driver: google-sa
    sa_file: /tmp/sa.json
    base_url: https://example.com/v1
    catalog: google-vertex
`)

	cfg, err := LoadUnifiedConfig(path, nil)
	require.NoError(t, err)
	assert.Equal(t, "google-vertex", cfg.Providers["vertex"].Catalog)
}

// Catalog metadata has exactly one source, so the fields that used to carry it by
// hand must fail loudly rather than being silently ignored.
func TestUnifiedConfig_ModelMetadataFieldsRejected(t *testing.T) {
	tests := []struct {
		name  string
		field string
	}{
		{"name", "name: Claude"},
		{"context_window", "context_window: 200000"},
		{"max_tokens", "max_tokens: 32000"},
		{"pricing", "pricing:\n      input_price: 3"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfig(t, `
providers:
  ant:
    driver: anthropic
    api_key: test
models:
  - id: claude-opus-4-6
    provider: ant
    `+tt.field+`
`)
			_, err := LoadUnifiedConfig(path, nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "not found in type config.ModelEntry")
		})
	}
}

// TestUnifiedConfig_EmbeddingKeyRejected asserts the removed embedding subsystem
// leaves no config surface: a config carrying embedding: now fails as an unknown
// field (unified YAML is strict).
func TestUnifiedConfig_EmbeddingKeyRejected(t *testing.T) {
	path := writeConfig(t, `
providers:
  openrouter:
    driver: openai
    api_key: sk-or-test
    base_url: https://openrouter.ai/api/v1

embedding:
  provider: openrouter
  model: text-embedding-3-small
`)

	_, err := LoadUnifiedConfig(path, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "embedding")
}

func TestUnifiedConfig_EmptyIsValid(t *testing.T) {
	path := writeConfig(t, `{}`)
	cfg, err := LoadUnifiedConfig(path, nil)
	require.NoError(t, err)
	assert.Empty(t, cfg.Providers)
	assert.Empty(t, cfg.Models)
	assert.False(t, cfg.Tools.Bash.Sandbox.Enabled)
	assert.Empty(t, cfg.Tools.Bash.Sandbox.WritablePaths)
}

func TestUnifiedConfig_BashSandbox(t *testing.T) {
	path := writeConfig(t, `
tools:
  bash:
    sandbox:
      enabled: true
      writable_paths:
        - ~/.cache
        - /tmp/build-cache
`)

	cfg, err := LoadUnifiedConfig(path, nil)
	require.NoError(t, err)
	assert.True(t, cfg.Tools.Bash.Sandbox.Enabled)
	assert.Equal(t, []string{"~/.cache", "/tmp/build-cache"}, cfg.Tools.Bash.Sandbox.WritablePaths)
}

func TestUnifiedConfig_RejectsUnknownBashSandboxField(t *testing.T) {
	path := writeConfig(t, `
tools:
  bash:
    sandbox:
      enable: true
`)

	_, err := LoadUnifiedConfig(path, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "field enable not found")
}

func TestUnifiedConfig_RejectsTrailingYAMLDocument(t *testing.T) {
	tests := map[string]string{
		"mapping": `{}
---
tools:
  bash:
    sandbox:
      enabled: true
`,
		"empty": `{}
---
`,
	}

	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			path := writeConfig(t, content)

			_, err := LoadUnifiedConfig(path, nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "multiple YAML documents are not supported")
		})
	}
}

func TestUnifiedConfig_ProviderMissingDriver(t *testing.T) {
	path := writeConfig(t, `
providers:
  foo:
    api_key: test
models: []
`)
	_, err := LoadUnifiedConfig(path, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `provider "foo" has no driver specified`)
}

func TestUnifiedConfig_ProviderUnknownDriver(t *testing.T) {
	path := writeConfig(t, `
providers:
  bar:
    driver: xyz
    api_key: test
`)
	_, err := LoadUnifiedConfig(path, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown driver "xyz"`)
}

func TestUnifiedConfig_ModelMissingProvider(t *testing.T) {
	path := writeConfig(t, `
providers:
  openrouter:
    driver: openai
    api_key: test
models:
  - id: gpt-5
`)
	_, err := LoadUnifiedConfig(path, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `model "gpt-5" missing required field "provider"`)
}

func TestUnifiedConfig_ModelUnknownProvider(t *testing.T) {
	path := writeConfig(t, `
providers:
  openrouter:
    driver: openai
    api_key: test
models:
  - id: claude-opus-4-6
    provider: anthropic-typo
`)
	_, err := LoadUnifiedConfig(path, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `model "claude-opus-4-6" references unknown provider "anthropic-typo"`)
}

func TestUnifiedConfig_ModelsWithoutProviders(t *testing.T) {
	path := writeConfig(t, `
models:
  - id: gpt-5
    provider: openrouter
`)
	_, err := LoadUnifiedConfig(path, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "models are configured but no providers defined")
}

func TestUnifiedConfig_AnthropicDriverRequiresAPIKey(t *testing.T) {
	path := writeConfig(t, `
providers:
  ant:
    driver: anthropic
`)
	_, err := LoadUnifiedConfig(path, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `requires "api_key"`)
}

func TestUnifiedConfig_GoogleSARequiresSAFileAndBaseURL(t *testing.T) {
	path := writeConfig(t, `
providers:
  vertex:
    driver: google-sa
    sa_file: /tmp/sa.json
`)
	_, err := LoadUnifiedConfig(path, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `requires "base_url"`)
}

// Limits are no longer a config concern — enrichment decides whether a model can
// be served, so loading a bare [id, provider] entry must succeed here.
func TestUnifiedConfig_ModelEntryNeedsOnlyIDAndProvider(t *testing.T) {
	path := writeConfig(t, `
providers:
  ant:
    driver: anthropic
    api_key: test
models:
  - id: claude-opus-4-6
    provider: ant
`)
	cfg, err := LoadUnifiedConfig(path, nil)
	require.NoError(t, err)
	assert.Zero(t, cfg.Models[0].MaxTokens)
	assert.Zero(t, cfg.Models[0].ContextWindow)
}

func TestUnifiedConfig_SecretExpansion(t *testing.T) {
	path := writeConfig(t, `
providers:
  openrouter:
    driver: openai
    api_key: ${TEST_API_KEY}
`)
	cfg, err := LoadUnifiedConfig(path, Secrets{"TEST_API_KEY": "expanded-key"})
	require.NoError(t, err)
	assert.Equal(t, "expanded-key", cfg.Providers["openrouter"].APIKey)
}

func TestUnifiedConfig_SecretIgnoresProcessEnv(t *testing.T) {
	t.Setenv("TEST_API_KEY", "from-environment")
	path := writeConfig(t, `
providers:
  openrouter:
    driver: openai
    api_key: ${TEST_API_KEY}
`)
	_, err := LoadUnifiedConfig(path, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "undefined TEST_API_KEY")
}

func TestUnifiedConfig_SecretUndefinedNamesVariable(t *testing.T) {
	path := writeConfig(t, `
providers:
  openrouter:
    driver: openai
    api_key: ${MISSING_KEY}
`)
	_, err := LoadUnifiedConfig(path, Secrets{"OTHER_KEY": "x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `provider "openrouter" api_key`)
	assert.Contains(t, err.Error(), "undefined MISSING_KEY")
}

func TestUnifiedConfig_SecretLiteralDollarPreserved(t *testing.T) {
	path := writeConfig(t, `
providers:
  openrouter:
    driver: openai
    api_key: sk-literal-a$bc
`)
	cfg, err := LoadUnifiedConfig(path, Secrets{"bc": "should-not-be-used"})
	require.NoError(t, err)
	assert.Equal(t, "sk-literal-a$bc", cfg.Providers["openrouter"].APIKey)
}

func TestUnifiedConfig_SecretNonSinkFieldStaysLiteral(t *testing.T) {
	path := writeConfig(t, `
providers:
  openrouter:
    driver: openai
    api_key: key
    base_url: ${SHOULD_STAY_LITERAL}
`)
	cfg, err := LoadUnifiedConfig(path, Secrets{"SHOULD_STAY_LITERAL": "resolved"})
	require.NoError(t, err)
	assert.Equal(t, "${SHOULD_STAY_LITERAL}", cfg.Providers["openrouter"].BaseURL)
}

// The mcp: section is gone entirely — server definitions live in SQLite now, and
// an old config carrying it must fail loudly rather than be silently ignored.
func TestUnifiedConfig_MCPSectionRejected(t *testing.T) {
	path := writeConfig(t, `
mcp:
  servers:
    datadog:
      command: datadog-mcp
`)
	_, err := LoadUnifiedConfig(path, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "field mcp not found")
}

func TestUnifiedConfig_SecretEmbeddedInLargerValue(t *testing.T) {
	path := writeConfig(t, `
providers:
  openrouter:
    driver: openai
    api_key: Bearer ${TOKEN}-suffix
`)
	cfg, err := LoadUnifiedConfig(path, Secrets{"TOKEN": "abc"})
	require.NoError(t, err)
	assert.Equal(t, "Bearer abc-suffix", cfg.Providers["openrouter"].APIKey)
}

func TestUnifiedConfig_SecretValueWithYAMLMetacharacters(t *testing.T) {
	path := writeConfig(t, `
providers:
  openrouter:
    driver: openai
    api_key: ${TRICKY}
`)
	tricky := "line1\nmodels:\n  - id: injected # comment: yes"

	cfg, err := LoadUnifiedConfig(path, Secrets{"TRICKY": tricky})
	require.NoError(t, err)
	assert.Equal(t, tricky, cfg.Providers["openrouter"].APIKey)
	assert.Empty(t, cfg.Models)
}

func TestUnifiedConfig_OpenRouterDriverValid(t *testing.T) {
	path := writeConfig(t, `
providers:
  or:
    driver: openrouter
    api_key: sk-or-test
    base_url: https://openrouter.ai/api/v1
models:
  - id: openai/gpt-4o
    provider: or
`)
	cfg, err := LoadUnifiedConfig(path, nil)
	require.NoError(t, err)
	assert.Equal(t, "openrouter", cfg.Providers["or"].Driver)
}

func TestUnifiedConfig_OpenRouterDriverRequiresAPIKey(t *testing.T) {
	path := writeConfig(t, `
providers:
  or:
    driver: openrouter
    base_url: https://openrouter.ai/api/v1
`)
	_, err := LoadUnifiedConfig(path, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `requires "api_key"`)
}

func TestUnifiedConfig_OpenRouterDriverRequiresBaseURL(t *testing.T) {
	path := writeConfig(t, `
providers:
  or:
    driver: openrouter
    api_key: sk-or-test
`)
	_, err := LoadUnifiedConfig(path, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `requires "base_url"`)
}

func TestUnifiedConfig_Managers_ValidTelegramWithDefaults(t *testing.T) {
	path := writeConfig(t, `
providers:
  openai:
    driver: openai
    api_key: sk-test
managers:
  - id: telegram-main
    driver: telegram
    enabled: true
    bot_token: tg-token
    allowed_user_ids: [123]
    target_chat_id: -100123456
    whisper:
      provider: openai
      model: whisper-1
`)

	cfg, err := LoadUnifiedConfig(path, nil)
	require.NoError(t, err)
	require.Len(t, cfg.Managers, 1)

	m := cfg.Managers[0]
	require.NotNil(t, m.Enabled)
	assert.True(t, *m.Enabled)
	assert.Equal(t, defaultTelegramServiceTopicName, m.ServiceTopicName)
	assert.Equal(t, defaultTelegramServiceTopicIconEmojiID, m.ServiceTopicIconEmojiID)
	assert.Equal(t, defaultTelegramSessionTopicIconEmojiID, m.SessionTopicIconEmojiID)
	assert.Equal(t, defaultTelegramSendChunkDelayMS, m.SendChunkDelayMS)
	assert.Equal(t, defaultTelegramPollTimeoutSec, m.PollTimeoutSec)
}

func TestUnifiedConfig_Managers_MissingEnabled(t *testing.T) {
	path := writeConfig(t, `
managers:
  - id: telegram-main
    driver: telegram
    bot_token: tg-token
    allowed_user_ids: [123]
    target_chat_id: -100123456
`)

	_, err := LoadUnifiedConfig(path, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `requires "enabled"`)
}

func TestUnifiedConfig_Managers_MissingTelegramFields(t *testing.T) {
	path := writeConfig(t, `
managers:
  - id: telegram-main
    driver: telegram
    enabled: true
    allowed_user_ids: [123]
    target_chat_id: -100123456
`)

	_, err := LoadUnifiedConfig(path, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `requires "bot_token"`)
}

func TestUnifiedConfig_Managers_UnknownDriver(t *testing.T) {
	path := writeConfig(t, `
managers:
  - id: manager-x
    driver: slack
    enabled: true
`)

	_, err := LoadUnifiedConfig(path, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown driver "slack"`)
}

func TestUnifiedConfig_Managers_DuplicateIDs(t *testing.T) {
	path := writeConfig(t, `
managers:
  - id: telegram-main
    driver: telegram
    enabled: true
    bot_token: tg-token
    allowed_user_ids: [123]
    target_chat_id: -100123456
  - id: telegram-main
    driver: telegram
    enabled: true
    bot_token: tg-token-2
    allowed_user_ids: [456]
    target_chat_id: -100654321
`)

	_, err := LoadUnifiedConfig(path, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `duplicate manager id "telegram-main"`)
}

func TestUnifiedConfig_Managers_WhisperUnknownProvider(t *testing.T) {
	path := writeConfig(t, `
providers:
  openai:
    driver: openai
    api_key: sk-test
managers:
  - id: telegram-main
    driver: telegram
    enabled: true
    bot_token: tg-token
    allowed_user_ids: [123]
    target_chat_id: -100123456
    whisper:
      provider: does-not-exist
      model: whisper-1
`)

	_, err := LoadUnifiedConfig(path, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `whisper references unknown provider "does-not-exist"`)
}

func TestUnifiedConfig_Managers_SecretExpansion(t *testing.T) {
	path := writeConfig(t, `
managers:
  - id: telegram-main
    driver: telegram
    enabled: true
    bot_token: ${TG_MAIN_BOT_TOKEN}
    allowed_user_ids: [123]
    target_chat_id: -100123456
`)

	cfg, err := LoadUnifiedConfig(path, Secrets{"TG_MAIN_BOT_TOKEN": "telegram-secret"})
	require.NoError(t, err)
	assert.Equal(t, "telegram-secret", cfg.Managers[0].BotToken)
}

func TestUnifiedConfig_Managers_SecretMissingFailsWithVariableName(t *testing.T) {
	path := writeConfig(t, `
managers:
  - id: telegram-main
    driver: telegram
    enabled: true
    bot_token: ${TG_MAIN_BOT_TOKEN}
    allowed_user_ids: [123]
    target_chat_id: -100123456
`)

	_, err := LoadUnifiedConfig(path, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `manager "telegram-main" bot_token`)
	assert.Contains(t, err.Error(), "undefined TG_MAIN_BOT_TOKEN")
}
