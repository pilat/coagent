package llm

import (
	"encoding/base64"
	"encoding/json"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
)

func (c *openAIResponsesClient) convertInput(messages []llmwire.Message) []any {
	items := make([]any, 0, len(messages))

	for _, msg := range messages {
		switch msg.Role {
		case roleUser:
			items = append(items, map[string]any{msgKeyRole: roleUser, msgKeyContent: c.inputContent(msg)})
		case roleAssistant:
			items = append(items, c.assistantItems(msg)...)
		case roleTool:
			items = append(items, c.toolOutputItem(msg))
		}
	}

	return items
}

// toolOutputItem renders a legacy result row without a call id as user text:
// a function_call_output cannot exist without one, and its content must survive.
func (c *openAIResponsesClient) toolOutputItem(msg llmwire.Message) map[string]any {
	if msg.ToolCallID == "" {
		return map[string]any{msgKeyRole: roleUser, msgKeyContent: c.inputContent(msg)}
	}

	return map[string]any{
		msgKeyType: respItemCallOutput,
		"call_id":  msg.ToolCallID,
		"output":   c.inputContent(msg),
	}
}

// inputContent is plain text without images, else input parts: text first,
// then each image slot in order.
func (c *openAIResponsesClient) inputContent(msg llmwire.Message) any {
	if len(msg.Images) == 0 {
		return msg.Content
	}

	parts := make([]map[string]any, 0, len(msg.Images)+1)
	if msg.Content != "" {
		parts = append(parts, responsesTextPart(msg.Content))
	}

	log := logger.Named("llm.client")

	for _, ref := range msg.Images {
		data, reason := resolveImage(c.inputModalities, ref, c.imageAuthorizer, log)
		if data == nil {
			parts = append(parts, responsesTextPart(llmwire.ImagePlaceholder(reason)))

			continue
		}

		parts = append(parts, map[string]any{
			msgKeyType:     respPartInputImage,
			msgKeyImageURL: "data:" + ref.Mime + ";base64," + base64.StdEncoding.EncodeToString(data),
		})
	}

	return parts
}

func responsesTextPart(text string) map[string]any {
	return map[string]any{msgKeyType: respPartInputText, oaiTypeText: text}
}

func (c *openAIResponsesClient) assistantItems(msg llmwire.Message) []any {
	if items, ok := c.replayItems(msg); ok {
		return items
	}

	items := make([]any, 0, len(msg.ToolCalls)+1)
	if msg.Content != "" {
		items = append(items, map[string]any{msgKeyRole: roleAssistant, msgKeyContent: msg.Content})
	}

	for _, tc := range msg.ToolCalls {
		items = append(items, functionCallItem(tc, ""))
	}

	return items
}

// replayItems rebuilds the response's output order from its envelope, or
// reports false when the envelope does not fit this message exactly.
func (c *openAIResponsesClient) replayItems(msg llmwire.Message) ([]any, bool) {
	skeleton, ok := c.openSkeleton(msg.ReasoningRaw)
	if !ok {
		return nil, false
	}

	calls := make(map[string]llmwire.ToolCall, len(msg.ToolCalls))
	for _, tc := range msg.ToolCalls {
		calls[tc.ID] = tc
	}

	used := make(map[string]bool, len(msg.ToolCalls))
	items := make([]any, 0, len(skeleton.Items)+len(msg.ToolCalls))
	hasMessage := false

	for i, entry := range skeleton.Items {
		item, fits := replayEntry(skeleton.Items, i, calls, used)
		if !fits {
			return nil, false
		}

		items = append(items, item)
		hasMessage = hasMessage || entry.Kind == respItemMessage
	}

	// Text is taken only from stored message items; a mismatch would drop it.
	if !hasMessage && msg.Content != "" {
		return nil, false
	}

	for _, tc := range msg.ToolCalls {
		if !used[tc.ID] {
			items = append(items, functionCallItem(tc, ""))
		}
	}

	return items, true
}

func (c *openAIResponsesClient) openSkeleton(raw json.RawMessage) (responsesSkeleton, bool) {
	var skeleton responsesSkeleton

	payload, ok := unwrapReasoning(raw, c.model)
	if !ok {
		return skeleton, false
	}

	if err := json.Unmarshal(payload, &skeleton); err != nil || skeleton.Format != responsesSkeletonFormat {
		return skeleton, false
	}

	return skeleton, true
}

// replayEntry renders entries[i], marking the call it consumes in used; false
// means the entry cannot be replayed faithfully.
func replayEntry(
	entries []skeletonEntry,
	i int,
	calls map[string]llmwire.ToolCall,
	used map[string]bool,
) (any, bool) {
	entry := entries[i]

	switch entry.Kind {
	case respItemReasoning:
		if len(entry.Raw) == 0 || i+1 >= len(entries) || !followsReasoning(entries[i+1].Kind) {
			return nil, false
		}

		return entry.Raw, true
	case respItemMessage:
		return entry.Raw, len(entry.Raw) > 0
	case respItemFunctionCall:
		tc, found := calls[entry.CallID]
		if !found || used[entry.CallID] {
			return nil, false
		}

		used[entry.CallID] = true

		return functionCallItem(tc, entry.ID), true
	default:
		return nil, false
	}
}

func followsReasoning(kind string) bool {
	return kind == respItemMessage || kind == respItemFunctionCall
}

func functionCallItem(tc llmwire.ToolCall, itemID string) map[string]any {
	item := map[string]any{
		msgKeyType:  respItemFunctionCall,
		"call_id":   tc.ID,
		msgKeyName:  tc.Name,
		"arguments": string(tc.Arguments),
	}
	if itemID != "" {
		item["id"] = itemID
	}

	return item
}
