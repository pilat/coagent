package llm

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/llmwire"
)

func (c *openAIResponsesClient) parseResponsesBody(
	log *zap.Logger,
	body []byte,
	start time.Time,
) (*llmwire.Response, error) {
	var reply responsesReply
	if err := json.Unmarshal(body, &reply); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	if reply.Status == respStatusFailed || reply.Error != nil {
		log.Warn("response_failed", zap.String("provider", c.provider), zap.String("model", c.model),
			zap.String("raw", truncateBody(body)))

		return nil, fmt.Errorf("%s api response failed: %s", c.provider, reply.failureMessage())
	}

	items, kinds := decodeOutput(log, reply.Output)

	result := &llmwire.Response{}

	var texts []string

	for _, item := range items {
		switch item.Type {
		case respItemMessage:
			texts = append(texts, messageText(item)...)
		case respItemFunctionCall:
			if item.Name != "" {
				result.ToolCalls = append(result.ToolCalls, llmwire.ToolCall{
					ID: item.CallID, Name: item.Name, Arguments: []byte(item.Arguments),
				})
			}
		}
	}

	result.Text = strings.Join(texts, "\n")
	result.FinishType, result.ProviderFinishReason = reply.finish(len(result.ToolCalls) > 0)
	result.ReasoningRaw = c.sealSkeleton(reply.Output, items, kinds)

	c.logResponse(log, result.ProviderFinishReason, result, time.Since(start).Milliseconds())
	attachUsage(result, c.responsesUsage(reply.Usage))

	return result, nil
}

func (r *responsesReply) failureMessage() string {
	if r.Error == nil || r.Error.Message == "" {
		return "status " + r.Status
	}

	if r.Error.Code == "" {
		return r.Error.Message
	}

	return r.Error.Code + ": " + r.Error.Message
}

// finish decides by status first: an incomplete response never reports tool
// calls, so a truncated call's arguments are never executed.
func (r *responsesReply) finish(hasCalls bool) (string, string) {
	switch r.Status {
	case respStatusIncomplete:
		if r.IncompleteDetails == nil || r.IncompleteDetails.Reason == "" {
			return llmwire.FinishUnknown, respStatusIncomplete
		}

		reason := respStatusIncomplete + ":" + r.IncompleteDetails.Reason
		if r.IncompleteDetails.Reason == respIncompleteMaxOutput {
			return llmwire.FinishLength, reason
		}

		return llmwire.FinishUnknown, reason
	case respStatusCompleted:
		if hasCalls {
			return llmwire.FinishToolCalls, r.Status
		}

		return llmwire.FinishStop, r.Status
	default:
		return llmwire.FinishUnknown, r.Status
	}
}

// decodeOutput parses each output item; kinds keeps the raw type of every
// item, including ones that failed to decode, so the store rule sees them all.
func decodeOutput(log *zap.Logger, output []json.RawMessage) ([]responsesItem, []string) {
	items := make([]responsesItem, len(output))
	kinds := make([]string, len(output))

	for i, raw := range output {
		if err := json.Unmarshal(raw, &items[i]); err != nil {
			log.Debug("output_item_undecodable", zap.Error(err))

			continue
		}

		kinds[i] = items[i].Type

		switch items[i].Type {
		case respItemReasoning, respItemMessage, respItemFunctionCall:
		default:
			log.Debug("output_item_ignored", zap.String("type", items[i].Type))
		}
	}

	return items, kinds
}

func messageText(item responsesItem) []string {
	var texts []string

	for _, part := range item.Content {
		switch {
		case part.Type == respPartOutputText && part.Text != "":
			texts = append(texts, part.Text)
		case part.Type == respPartRefusal && part.Refusal != "":
			texts = append(texts, part.Refusal)
		}
	}

	return texts
}

// sealSkeleton stores the output order only when replay can honour it: at least one
// reasoning item, each encrypted and directly followed by a message or call, nothing unknown.
func (c *openAIResponsesClient) sealSkeleton(
	output []json.RawMessage,
	items []responsesItem,
	kinds []string,
) json.RawMessage {
	hasReasoning := false

	for i, kind := range kinds {
		switch kind {
		case respItemReasoning:
			// Under store:false an id-only reasoning item cannot be resolved on replay.
			if items[i].EncryptedContent == "" || i+1 >= len(kinds) || !followsReasoning(kinds[i+1]) {
				return nil
			}

			hasReasoning = true
		case respItemMessage, respItemFunctionCall:
		default:
			return nil
		}
	}

	if !hasReasoning {
		return nil
	}

	skeleton := responsesSkeleton{Format: responsesSkeletonFormat, Items: make([]skeletonEntry, len(items))}

	for i, item := range items {
		if item.Type == respItemFunctionCall {
			skeleton.Items[i] = skeletonEntry{Kind: item.Type, ID: item.ID, CallID: item.CallID}

			continue
		}

		skeleton.Items[i] = skeletonEntry{Kind: item.Type, Raw: output[i]}
	}

	payload, err := json.Marshal(skeleton)
	if err != nil {
		return nil
	}

	return wrapReasoning(c.model, payload)
}

// responsesUsage maps OpenAI's accounting, whose input_tokens already includes
// cached tokens, onto the cache-inclusive Usage contract.
func (c *openAIResponsesClient) responsesUsage(u responsesUsage) Usage {
	usage := Usage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.TotalTokens,
	}
	if u.InputTokensDetails != nil {
		usage.CacheTokens = u.InputTokensDetails.CachedTokens
	}

	usage.CostUSD = estimateCost(usage, c.pricing)

	return usage
}
