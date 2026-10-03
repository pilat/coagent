package sessionprompt

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pilat/coagent/internal/config"
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
