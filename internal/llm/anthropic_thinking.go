package llm

import (
	"encoding/json"
	"math"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/pilat/coagent/internal/catalog"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llmwire"
)

const (
	thinkingBlockType         = "thinking"
	redactedThinkingBlockType = "redacted_thinking"
)

// anthropicThinkingBlock is the wire shape of a thinking block. Our own type, so a
// payload persisted months ago still replays across SDK churn.
type anthropicThinkingBlock struct {
	Type      string `json:"type"`
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"`
}

// budgetFractions follow OpenRouter's documented effort convention, so budget-based
// models behave like the effort-native ones rather than a number we invented.
var budgetFractions = map[ReasoningLevel]float64{
	ReasoningMinimal: 0.1,
	ReasoningLow:     0.2,
	ReasoningMedium:  0.5,
	ReasoningHigh:    0.8,
	ReasoningXHigh:   0.95,
	ReasoningMax:     0.95,
}

// thinkingParams is what a request carries to ask for reasoning: adaptive thinking
// plus an effort level for models that take one, a token budget for the rest.
type thinkingParams struct {
	Thinking anthropic.ThinkingConfigParamUnion
	Effort   anthropic.OutputConfigEffort
	Enabled  bool
}

// buildThinkingParams maps a catalog reasoning spec and an effort level onto the
// request shape the model accepts. A model that cannot reason gets nothing at all.
func buildThinkingParams(spec *config.ReasoningSpec, level ReasoningLevel, maxTokens int) thinkingParams {
	if spec == nil || !spec.Supported {
		return thinkingParams{}
	}

	if spec.NativeEffort {
		return thinkingParams{
			Thinking: anthropic.ThinkingConfigParamUnion{OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{}},
			Effort:   anthropic.OutputConfigEffort(catalog.ClampEffort(string(level), spec.Efforts)),
			Enabled:  true,
		}
	}

	budget, ok := thinkingBudget(spec, level, maxTokens)
	if !ok {
		return thinkingParams{}
	}

	return thinkingParams{
		Thinking: anthropic.ThinkingConfigParamOfEnabled(int64(budget)),
		Enabled:  true,
	}
}

// thinkingBudget sizes budget_tokens. The ceiling wins over the catalog floor —
// the API rejects budget >= max_tokens — and inverted bounds mean no thinking.
func thinkingBudget(spec *config.ReasoningSpec, level ReasoningLevel, maxTokens int) (int, bool) {
	fraction, ok := budgetFractions[level]
	if !ok {
		fraction = budgetFractions[ReasoningMedium]
	}

	ceiling := maxTokens - 1
	if ceiling < 1 {
		return 0, false
	}

	budget := int(math.Round(fraction * float64(maxTokens)))
	budget = max(budget, spec.BudgetMin)
	budget = min(budget, ceiling)

	if budget < spec.BudgetMin {
		return 0, false
	}

	return budget, true
}

// replayThinkingBlocks rebuilds a stored assistant turn's thinking blocks. They go
// back only to the model that signed them; anything else is dropped silently.
func (c *anthropicClient) replayThinkingBlocks(msg llmwire.Message) []anthropic.ContentBlockParamUnion {
	payload, ok := unwrapReasoning(msg.ReasoningRaw, c.model)
	if !ok {
		return nil
	}

	var stored []anthropicThinkingBlock
	if err := json.Unmarshal(payload, &stored); err != nil {
		return nil
	}

	blocks := make([]anthropic.ContentBlockParamUnion, 0, len(stored))

	for _, b := range stored {
		switch b.Type {
		case thinkingBlockType:
			blocks = append(blocks, anthropic.NewThinkingBlock(b.Signature, b.Thinking))
		case redactedThinkingBlockType:
			blocks = append(blocks, anthropic.NewRedactedThinkingBlock(b.Data))
		}
	}

	return blocks
}
