package llm

import (
	"strings"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
)

// mapOpenAIFinish translates the OpenAI-compatible finish_reason onto the
// portable llmwire outcome; anything undocumented is unknown, never stop.
func mapOpenAIFinish(reason string) string {
	switch reason {
	case "stop":
		return llmwire.FinishStop
	case "length":
		return llmwire.FinishLength
	case "tool_calls", "function_call":
		return llmwire.FinishToolCalls
	default:
		return llmwire.FinishUnknown
	}
}

func (c *openaiClient) parseMessage(message *oaiMessage, finishReason string) (*llmwire.Response, error) {
	resp := &llmwire.Response{
		FinishType: mapOpenAIFinish(finishReason),
	}

	if message == nil {
		return resp, nil
	}

	resp.Text = message.content()

	if message.ReasoningContent != nil && *message.ReasoningContent != "" {
		resp.ReasoningContent = *message.ReasoningContent
	}

	// DeepSeek-R1 returns reasoning in <think>...</think> tags within content
	if resp.ReasoningContent == "" && strings.Contains(resp.Text, "<think>") {
		resp.ReasoningContent = extractThinkContent(resp.Text)
		resp.Text = removeThinkTags(resp.Text)
	}

	resp.ReasoningRaw = wrapReasoning(c.model, message.ReasoningDetails)

	for _, tc := range message.ToolCalls {
		if tc.Function.Name != "" {
			resp.ToolCalls = append(resp.ToolCalls, llmwire.ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: []byte(tc.Function.Arguments),
			})
			resp.FinishType = llmwire.FinishToolCalls
		}
	}

	return resp, nil
}

// extractThinkContent extracts content from <think>...</think> tags.
func extractThinkContent(text string) string {
	start := strings.Index(text, "<think>")
	end := strings.Index(text, "</think>")

	if start == -1 || end == -1 || end <= start {
		return ""
	}

	return strings.TrimSpace(text[start+7 : end])
}

// removeThinkTags removes <think>...</think> and surrounding whitespace from text.
func removeThinkTags(text string) string {
	start := strings.Index(text, "<think>")
	end := strings.Index(text, "</think>")

	if start == -1 || end == -1 || end <= start {
		return text
	}
	// Remove the think block and clean up surrounding whitespace
	before := strings.TrimRightFunc(text[:start], func(r rune) bool { return r == '\n' || r == ' ' || r == '\t' })
	after := strings.TrimLeftFunc(text[end+8:], func(r rune) bool { return r == '\n' || r == ' ' || r == '\t' })

	if before == "" {
		return after
	}

	if after == "" {
		return before
	}

	return before + "\n\n" + after
}

// extractUsage converts oaiResponse usage into the internal Usage type.
// If the provider returned a cost (e.g., OpenRouter), it is used directly.
// Otherwise the call is priced from the model's catalog rates.
func extractUsage(resp *oaiResponse, provider, model string, pricing *config.ModelPricing) Usage {
	usage := Usage{
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
		TotalTokens:      resp.Usage.TotalTokens,
	}
	if resp.Usage.PromptTokensDetails != nil {
		usage.CacheTokens = resp.Usage.PromptTokensDetails.CachedTokens
		usage.CacheWriteTokens = resp.Usage.PromptTokensDetails.CacheWriteTokens
	}

	// Canary for a provider that excludes cache from prompt_tokens: add it back so
	// the cache-inclusive invariant holds. Empirically dormant on today's providers.
	if cacheSum := usage.CacheTokens + usage.CacheWriteTokens; cacheSum > usage.PromptTokens {
		logger.Named("llm.openai").Warn("prompt_tokens_excludes_cache",
			zap.String("provider", provider), zap.String("model", model),
			zap.Int("prompt_tokens", usage.PromptTokens), zap.Int("cache_sum", cacheSum))
		usage.PromptTokens += cacheSum
	}

	if resp.Usage.Cost != nil {
		usage.CostUSD = *resp.Usage.Cost
	} else {
		usage.CostUSD = estimateCost(usage, pricing)
	}

	return usage
}

// ensureSchemaProperties adds empty "properties" to object schemas that lack it.
// OpenAI/Azure strictly validates function schemas and rejects objects without properties.
func ensureSchemaProperties(schema map[string]any) {
	if schema == nil {
		return
	}

	typ, _ := schema["type"].(string)
	if typ == "object" {
		if _, ok := schema["properties"]; !ok {
			schema["properties"] = map[string]any{}
		}
	}
}
