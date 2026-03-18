package llm

import (
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/logger"
)

// TestAnthropicUsage_PromptTokensCacheInclusive pins the Anthropic mapping:
// PromptTokens sums the uncached remainder with both cache breakdowns.
func TestAnthropicUsage_PromptTokensCacheInclusive(t *testing.T) {
	c := &anthropicClient{model: "claude-opus-4-6"}

	var message anthropic.Message
	message.Usage.InputTokens = 1000
	message.Usage.CacheReadInputTokens = 50000
	message.Usage.CacheCreationInputTokens = 2000
	message.Usage.OutputTokens = 300

	usage := c.extractAnthropicUsage(&message)

	assert.Equal(t, 53000, usage.PromptTokens)
	assert.Equal(t, 50000, usage.CacheTokens)
	assert.Equal(t, 2000, usage.CacheWriteTokens)
	assert.Equal(t, 300, usage.CompletionTokens)
	assert.Equal(t, 53300, usage.TotalTokens)
	assert.GreaterOrEqual(t, usage.PromptTokens, usage.CacheTokens+usage.CacheWriteTokens,
		"invariant: PromptTokens >= CacheTokens + CacheWriteTokens")
}

// TestOpenAIUsage_PromptTokensPassThrough pins the OpenAI-compat mapping: the
// provider already reports cache-inclusive prompt_tokens, so it passes through.
func TestOpenAIUsage_PromptTokensPassThrough(t *testing.T) {
	resp := &oaiResponse{}
	resp.Usage.PromptTokens = 53000
	resp.Usage.CompletionTokens = 300
	resp.Usage.PromptTokensDetails = &oaiPromptTokensDetails{CachedTokens: 50000}

	usage := extractUsage(resp, "openrouter", "some-model", nil)

	assert.Equal(t, 53000, usage.PromptTokens)
	assert.Equal(t, 50000, usage.CacheTokens)
	assert.GreaterOrEqual(t, usage.PromptTokens, usage.CacheTokens+usage.CacheWriteTokens,
		"invariant: PromptTokens >= CacheTokens + CacheWriteTokens")
}

// TestOpenAIUsage_GuardAddsBackExcludedCache exercises the canary: a provider
// that reports cache-excluding prompt_tokens gets the cache added back and warns.
func TestOpenAIUsage_GuardAddsBackExcludedCache(t *testing.T) {
	core, logs := observer.New(zapcore.WarnLevel)
	prev := logger.L
	logger.L = zap.New(core)

	t.Cleanup(func() { logger.L = prev })

	resp := &oaiResponse{}
	resp.Usage.PromptTokens = 100
	resp.Usage.PromptTokensDetails = &oaiPromptTokensDetails{CachedTokens: 50000, CacheWriteTokens: 2000}

	usage := extractUsage(resp, "future-provider", "future-model", nil)

	assert.Equal(t, 52100, usage.PromptTokens)
	assert.Equal(t, 1, logs.FilterMessage("prompt_tokens_excludes_cache").Len(), "canary warn must fire")
}

// TestEstimateCost_AnthropicFreshInputNotZeroed proves the fresh (uncached) input
// is billed post-fix. Pre-fix, PromptTokens=input-only clamped effectiveInput to 0.
func TestEstimateCost_AnthropicFreshInputNotZeroed(t *testing.T) {
	usage := Usage{
		PromptTokens:     53000, // 1000 fresh + 50000 read + 2000 write
		CompletionTokens: 0,
		CacheTokens:      50000,
		CacheWriteTokens: 2000,
	}

	// InputPrice=1, all cache/output prices 0 → cost == effectiveInput / 1e6.
	cost := estimateCost(usage, &config.ModelPricing{InputPrice: 1.0})

	// effectiveInput = 53000 - 50000 - 2000 = 1000; cost = 1000 * 1 / 1e6.
	assert.InDelta(t, 1000.0/1_000_000, cost, 1e-12)
}
