package llm

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llmwire"
)

// The option lowers the client's own limit and never raises it.
func TestEffectiveMaxTokensTakesTheLowerOfTheTwo(t *testing.T) {
	capped := llmwire.ApplyChatOptions([]llmwire.ChatOption{llmwire.WithMaxTokens(8000)})

	assert.Equal(t, 8000, capped.EffectiveMaxTokens(64000))
	assert.Equal(t, 4000, capped.EffectiveMaxTokens(4000), "a tighter client limit still wins")
	// 0 means "unset, omit the field": min(0, cap) would silently apply nothing.
	assert.Equal(t, 8000, capped.EffectiveMaxTokens(0))

	none := llmwire.ApplyChatOptions(nil)
	assert.Equal(t, 64000, none.EffectiveMaxTokens(64000), "no option: byte-identical to before")
	assert.Equal(t, 0, none.EffectiveMaxTokens(0))
}

func TestOpenAIChatCarriesThePerCallCap(t *testing.T) {
	params := openAICompatibleParams{
		APIKey: "k",
		Model:  config.ModelEntry{ID: "m", MaxTokens: 64000, ContextWindow: 200000},
	}

	body := captureChatRequest(t, params, llmwire.WithMaxTokens(8000))
	assert.InDelta(t, 8000.0, body["max_tokens"], 0.5)

	uncapped := captureChatRequest(t, params)
	assert.InDelta(t, 30000.0, uncapped["max_tokens"], 0.5, "the client's own clamped limit")
}

// A catalog with no output limit leaves the client at 0 (field omitted). The cap
// must then become the limit, or compaction's cap would silently do nothing.
func TestOpenAIChatAppliesTheCapWhenTheClientHasNoLimit(t *testing.T) {
	body := captureChatRequest(t, openAICompatibleParams{
		APIKey: "k",
		Model:  config.ModelEntry{ID: "m"},
	}, llmwire.WithMaxTokens(8000))

	assert.InDelta(t, 8000.0, body["max_tokens"], 0.5)
}

// Anthropic rejects budget_tokens >= max_tokens, so a capped request must size
// its thinking against the cap — otherwise every compaction on a thinking model
// is a deterministic 400.
func TestAnthropicThinkingBudgetFollowsThePerCallCap(t *testing.T) {
	c := &anthropicClient{
		model:          "claude-test",
		maxTokens:      64000,
		reasoning:      &config.ReasoningSpec{Supported: true, BudgetMin: 1024},
		reasoningLevel: ReasoningHigh,
	}

	effective := llmwire.ApplyChatOptions([]llmwire.ChatOption{
		llmwire.WithMaxTokens(8000),
	}).EffectiveMaxTokens(c.maxTokens)
	require.Equal(t, 8000, effective)

	params := c.buildMessageParams("sys", nil, nil, effective)

	assert.Equal(t, int64(8000), params.MaxTokens)
	require.NotNil(t, params.Thinking.OfEnabled)
	assert.Less(t, params.Thinking.OfEnabled.BudgetTokens, params.MaxTokens,
		"budget must stay strictly under the cap the request carries")
}
