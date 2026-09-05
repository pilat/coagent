package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/tool"
)

// searchGuidanceConfig builds a unified config whose providers land on
// distinct drivers, so the native-search resolution per model is observable.
func searchGuidanceConfig() *config.UnifiedConfig {
	return &config.UnifiedConfig{
		Providers: map[string]config.ProviderEntry{
			"openrouter": {Driver: "openrouter", APIKey: "sk-test", BaseURL: "https://openrouter.ai/api/v1"},
			"anthropic":  {Driver: "anthropic", APIKey: "sk-ant"},
		},
		Models: []config.ModelEntry{
			{ID: "or-model", Name: "or-model", Provider: "openrouter"},
			{ID: "ant-model", Name: "ant-model", Provider: "anthropic"},
		},
	}
}

func registryWithTools(ids ...string) tool.Registry {
	reg := tool.NewRegistry()
	for _, id := range ids {
		reg.Register(&stubTool{id: id})
	}

	return reg
}

func TestBuildToolsSection_WebSearchBuiltinToolRegistered(t *testing.T) {
	t.Parallel()

	reg := registryWithTools("read", "webfetch", websearchToolName)

	result := buildToolsSection(reg, false)

	assert.Contains(t, result, "# WEB SEARCH")
	assert.Contains(t, result, "You have web search capability via: websearch")
	assert.Contains(t, result, "Web: webfetch (fetch known URL), websearch (web search)")
	assert.Contains(t, result, "Sources:")
}

func TestBuildToolsSection_WebSearchNativeActive(t *testing.T) {
	t.Parallel()

	reg := registryWithTools("read", "webfetch")

	result := buildToolsSection(reg, true)

	assert.Contains(t, result, "# WEB SEARCH")
	assert.Contains(t, result, "provided natively by your model provider")
	assert.Contains(t, result, "No local search tool exists in this session")
	assert.NotContains(t, result, "You have web search capability via:")
	assert.Contains(t, result, "Do not guess URLs")
}

func TestBuildToolsSection_WebSearchNoneGuidanceAbsent(t *testing.T) {
	t.Parallel()

	reg := registryWithTools("read", "webfetch")

	result := buildToolsSection(reg, false)

	assert.NotContains(t, result, "# WEB SEARCH")
}

// The forward invariant: no search source, no guidance section — true before
// this change and pinned to stay true.
func TestBuildToolsSection_WebSearchEmptyRegistryNoGuidance(t *testing.T) {
	t.Parallel()

	reg := registryWithTools()

	result := buildToolsSection(reg, false)

	assert.NotContains(t, result, "# WEB SEARCH")
}

// Native-search resolution: explicit REST wins over native, disable removes
// everything, unconfigured falls back to the driver.
func TestNativeSearchActivePrecedence(t *testing.T) {
	t.Parallel()

	uc := searchGuidanceConfig()

	assert.True(t, uc.SearchNativeActive("or-model"), "unconfigured + OR driver = native")
	assert.False(t, uc.SearchNativeActive("ant-model"), "unconfigured + non-OR driver = nothing")

	tavily := &config.UnifiedConfig{}
	*tavily = *uc
	tavily.Tools.Search = config.SearchToolConfig{Provider: config.SearchProviderTavily, APIKey: "tvly-test"}
	assert.False(t, tavily.SearchNativeActive("or-model"), "explicit REST wins over native")

	disabled := &config.UnifiedConfig{}
	*disabled = *uc
	off := false
	disabled.Tools.Search.Enabled = &off
	assert.False(t, disabled.SearchNativeActive("or-model"), "explicit disable removes native")
}

// handleSetModel must move the search guidance with the client: switching
// between a native-capable (OR) and a non-native model flips the section.
func TestHandleSetModel_SearchGuidanceFollowsActiveClient(t *testing.T) {
	t.Parallel()

	t.Run("OR to non-OR drops the native section", func(t *testing.T) {
		ucOR := searchGuidanceConfig()
		cfgOR := &config.Config{UnifiedConfig: ucOR}

		s := &svc{
			cfg:       cfgOR,
			llmClient: &mockLLMClientTracked{model: "or-model"},
			model:     "or-model",
			prompt:    newPromptBuilder("", "", ""),
			ms:        newMessageStore(nil, 0, nil),
			registry:  registryWithTools("read", "webfetch"),
			newLLMWithModel: func(_ *config.Config, _ string) (llm.Client, error) {
				return &mockLLMClientTracked{model: "ant-model"}, nil
			},
		}
		s.prompt.setNativeSearch(ucOR.SearchNativeActive("or-model"))
		s.prompt.refreshToolsSection(s.registry)
		require.Contains(t, s.prompt.systemPrompt(), "provided natively by your model provider")

		require.NoError(t, s.handleSetModel("ant-model", ""))
		assert.NotContains(t, s.prompt.systemPrompt(), "# WEB SEARCH")
	})

	t.Run("non-OR to OR gains the native section", func(t *testing.T) {
		ucANT := searchGuidanceConfig()
		cfgANT := &config.Config{UnifiedConfig: ucANT}

		s := &svc{
			cfg:       cfgANT,
			llmClient: &mockLLMClientTracked{model: "ant-model"},
			model:     "ant-model",
			prompt:    newPromptBuilder("", "", ""),
			ms:        newMessageStore(nil, 0, nil),
			registry:  registryWithTools("read", "webfetch"),
			newLLMWithModel: func(_ *config.Config, _ string) (llm.Client, error) {
				return &mockLLMClientTracked{model: "or-model"}, nil
			},
		}
		s.prompt.setNativeSearch(ucANT.SearchNativeActive("ant-model"))
		s.prompt.refreshToolsSection(s.registry)
		require.NotContains(t, s.prompt.systemPrompt(), "# WEB SEARCH")

		require.NoError(t, s.handleSetModel("or-model", ""))
		assert.Contains(t, s.prompt.systemPrompt(), "# WEB SEARCH")
		assert.Contains(t, s.prompt.systemPrompt(), "provided natively by your model provider")
	})
}
