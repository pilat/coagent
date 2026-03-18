package llm

import (
	"encoding/json"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
)

// --- Message conversion tests ---

func TestAnthropicConvertMessages_UserMessage(t *testing.T) {
	msgs := []llmwire.Message{
		{Role: "user", Content: "hello"},
	}
	result := convertAnthropicMessages(msgs)
	require.Len(t, result, 1)
	assert.Equal(t, anthropic.MessageParamRoleUser, result[0].Role)
	require.Len(t, result[0].Content, 1)
	assert.Equal(t, "hello", result[0].Content[0].OfText.Text)
}

func TestAnthropicConvertMessages_EmptyUserSkipped(t *testing.T) {
	msgs := []llmwire.Message{
		{Role: "user", Content: ""},
	}
	result := convertAnthropicMessages(msgs)
	assert.Empty(t, result)
}

func TestAnthropicConvertMessages_AssistantTextOnly(t *testing.T) {
	msgs := []llmwire.Message{
		{Role: "assistant", Content: "I'll help you"},
	}
	result := convertAnthropicMessages(msgs)
	require.Len(t, result, 1)
	assert.Equal(t, anthropic.MessageParamRoleAssistant, result[0].Role)
	require.Len(t, result[0].Content, 1)
	assert.Equal(t, "I'll help you", result[0].Content[0].OfText.Text)
}

func TestAnthropicConvertMessages_AssistantToolCallOnly(t *testing.T) {
	msgs := []llmwire.Message{
		{
			Role:    "assistant",
			Content: "", // empty text — tool-call-only response
			ToolCalls: []llmwire.ToolCall{
				{ID: "tc_1", Name: "bash", Arguments: []byte(`{"command":"ls"}`)},
			},
		},
	}
	result := convertAnthropicMessages(msgs)
	require.Len(t, result, 1)
	// Should have exactly 1 block: tool_use (no empty text block)
	require.Len(t, result[0].Content, 1)
	assert.Nil(t, result[0].Content[0].OfText, "should not have text block for empty content")
	assert.NotNil(t, result[0].Content[0].OfToolUse)
	assert.Equal(t, "tc_1", result[0].Content[0].OfToolUse.ID)
	assert.Equal(t, "bash", result[0].Content[0].OfToolUse.Name)
}

func TestAnthropicConvertMessages_AssistantTextAndToolCalls(t *testing.T) {
	msgs := []llmwire.Message{
		{
			Role:    "assistant",
			Content: "Let me check",
			ToolCalls: []llmwire.ToolCall{
				{ID: "tc_1", Name: "read", Arguments: []byte(`{"path":"/tmp/f"}`)},
				{ID: "tc_2", Name: "bash", Arguments: []byte(`{"command":"pwd"}`)},
			},
		},
	}
	result := convertAnthropicMessages(msgs)
	require.Len(t, result, 1)
	// text + 2 tool_use blocks
	require.Len(t, result[0].Content, 3)
	assert.Equal(t, "Let me check", result[0].Content[0].OfText.Text)
	assert.Equal(t, "read", result[0].Content[1].OfToolUse.Name)
	assert.Equal(t, "bash", result[0].Content[2].OfToolUse.Name)
}

func TestAnthropicConvertMessages_ToolResult(t *testing.T) {
	msgs := []llmwire.Message{
		{Role: "tool", Content: `{"output":"file contents"}`, ToolCallID: "tc_1"},
	}
	result := convertAnthropicMessages(msgs)
	require.Len(t, result, 1)
	assert.Equal(t, anthropic.MessageParamRoleUser, result[0].Role)
	require.Len(t, result[0].Content, 1)
	assert.NotNil(t, result[0].Content[0].OfToolResult)
	assert.Equal(t, "tc_1", result[0].Content[0].OfToolResult.ToolUseID)
}

func TestAnthropicConvertMessages_ToolResultNonJSON(t *testing.T) {
	msgs := []llmwire.Message{
		{Role: "tool", Content: "plain text output", ToolCallID: "tc_1"},
	}
	result := convertAnthropicMessages(msgs)
	require.Len(t, result, 1)
	assert.NotNil(t, result[0].Content[0].OfToolResult)
	// Non-JSON content gets wrapped in {"output": "..."}
	assert.Contains(t, result[0].Content[0].OfToolResult.Content[0].OfText.Text, "plain text output")
}

// --- Cache marker tests ---

func TestCacheMarkers_SystemPrompt(t *testing.T) {
	block := anthropic.TextBlockParam{Text: "system prompt"}
	block.CacheControl = anthropic.NewCacheControlEphemeralParam()

	data, err := json.Marshal(block)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"cache_control"`)
	assert.Contains(t, string(data), `"ephemeral"`)
}

func TestCacheMarkers_SingleUserMessage(t *testing.T) {
	msgs := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock("hello")),
	}
	addAnthropicNativeCacheMarkers(msgs)

	// First (and only) user message gets cache_control
	assert.Equal(t, "ephemeral", string(msgs[0].Content[0].OfText.CacheControl.Type))
}

func TestCacheMarkers_TwoUserMessages_NoSlidingWindow(t *testing.T) {
	msgs := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock("first")),
		anthropic.NewAssistantMessage(anthropic.NewTextBlock("reply")),
		anthropic.NewUserMessage(anthropic.NewTextBlock("second")),
	}
	addAnthropicNativeCacheMarkers(msgs)

	// First user gets cache_control
	assert.Equal(t, "ephemeral", string(msgs[0].Content[0].OfText.CacheControl.Type))
	// Second user is the last — no sliding window (need 3+ user messages for that)
	assert.Empty(t, string(msgs[2].Content[0].OfText.CacheControl.Type))
}

func TestCacheMarkers_ThreeUserMessages_SlidingWindow(t *testing.T) {
	msgs := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock("first")),
		anthropic.NewAssistantMessage(anthropic.NewTextBlock("r1")),
		anthropic.NewUserMessage(anthropic.NewTextBlock("second")),
		anthropic.NewAssistantMessage(anthropic.NewTextBlock("r2")),
		anthropic.NewUserMessage(anthropic.NewTextBlock("third")),
	}
	addAnthropicNativeCacheMarkers(msgs)

	// First user: cache_control
	assert.Equal(t, "ephemeral", string(msgs[0].Content[0].OfText.CacheControl.Type))
	// Second user (second-to-last): sliding window cache_control
	assert.Equal(t, "ephemeral", string(msgs[2].Content[0].OfText.CacheControl.Type))
	// Third user (last): no cache_control
	assert.Empty(t, string(msgs[4].Content[0].OfText.CacheControl.Type))
}

func TestCacheMarkers_ToolResultMessage(t *testing.T) {
	msgs := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewToolResultBlock("tc_1", "result", false)),
	}
	addAnthropicNativeCacheMarkers(msgs)

	// Tool result block should get cache_control
	assert.Equal(t, "ephemeral", string(msgs[0].Content[0].OfToolResult.CacheControl.Type))
}

func TestCacheMarkers_EmptyMessages(t *testing.T) {
	var msgs []anthropic.MessageParam
	// Should not panic
	addAnthropicNativeCacheMarkers(msgs)
}

// --- Stream usage accumulation test ---

// Cache counts arrive on message_delta, not message_start. Accumulate is the only
// thing that merges them now, so this drives real stream events through it.
func TestStreamUsageAccumulatesCacheCountsFromDelta(t *testing.T) {
	var message anthropic.Message

	require.NoError(t, accumulateRaw(&message, `{
		"type": "message_start",
		"message": {
			"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-opus-5",
			"content": [], "stop_reason": null,
			"usage": {"input_tokens": 100, "output_tokens": 0}
		}
	}`))

	require.NoError(t, accumulateRaw(&message, `{
		"type": "message_delta",
		"delta": {"stop_reason": "end_turn"},
		"usage": {
			"input_tokens": 100, "output_tokens": 50,
			"cache_read_input_tokens": 8000, "cache_creation_input_tokens": 2000
		}
	}`))

	c := &anthropicClient{model: "claude-opus-5"}
	usage := c.extractAnthropicUsage(&message)

	assert.Equal(t, 10100, usage.PromptTokens, "prompt tokens are cache-inclusive")
	assert.Equal(t, 50, usage.CompletionTokens)
	assert.Equal(t, 8000, usage.CacheTokens)
	assert.Equal(t, 2000, usage.CacheWriteTokens)
}

// A delta that omits the input/cache counts must leave the message_start values
// alone rather than zeroing them.
func TestStreamUsageDeltaWithoutCountsKeepsStartValues(t *testing.T) {
	var message anthropic.Message

	require.NoError(t, accumulateRaw(&message, `{
		"type": "message_start",
		"message": {
			"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-opus-5",
			"content": [], "stop_reason": null,
			"usage": {"input_tokens": 100, "output_tokens": 0, "cache_read_input_tokens": 4000}
		}
	}`))

	require.NoError(t, accumulateRaw(&message, `{
		"type": "message_delta",
		"delta": {"stop_reason": "end_turn"},
		"usage": {"output_tokens": 50}
	}`))

	assert.Equal(t, int64(100), message.Usage.InputTokens)
	assert.Equal(t, int64(4000), message.Usage.CacheReadInputTokens)
	assert.Equal(t, int64(50), message.Usage.OutputTokens)
}

func accumulateRaw(message *anthropic.Message, raw string) error {
	var event anthropic.MessageStreamEventUnion
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		return err
	}

	return message.Accumulate(event)
}

// --- parseResponse tests ---

func TestParseResponse_TextOnly(t *testing.T) {
	c := &anthropicClient{}
	// Build a ContentBlockUnion that AsAny() can dispatch — must unmarshal from JSON.
	raw := `{"type":"text","text":"hello"}`
	var block anthropic.ContentBlockUnion
	require.NoError(t, block.UnmarshalJSON([]byte(raw)))

	msg := &anthropic.Message{Content: []anthropic.ContentBlockUnion{block}}

	resp, err := c.parseResponse(msg)
	require.NoError(t, err)
	assert.Equal(t, "hello", resp.Text)
	assert.Empty(t, resp.ToolCalls)
	assert.Equal(t, "stop", resp.FinishType)
}

func TestParseResponse_NilMessage(t *testing.T) {
	c := &anthropicClient{}
	resp, err := c.parseResponse(nil)
	require.NoError(t, err)
	assert.Equal(t, "stop", resp.FinishType)
	assert.Empty(t, resp.Text)
}

// --- Helper to extract message conversion logic for testing ---

func convertAnthropicMessages(messages []llmwire.Message) []anthropic.MessageParam {
	var result []anthropic.MessageParam

	for _, msg := range messages {
		switch msg.Role {
		case roleUser:
			if msg.Content != "" {
				result = append(result, anthropic.NewUserMessage(
					anthropic.NewTextBlock(msg.Content),
				))
			}
		case "assistant":
			var blocks []anthropic.ContentBlockParamUnion
			if msg.Content != "" {
				blocks = append(blocks, anthropic.NewTextBlock(msg.Content))
			}
			for _, tc := range msg.ToolCalls {
				var input map[string]any
				if err := json.Unmarshal(tc.Arguments, &input); err != nil {
					input = map[string]any{}
				}
				blocks = append(blocks, anthropic.NewToolUseBlock(tc.ID, input, tc.Name))
			}
			if len(blocks) > 0 {
				result = append(result, anthropic.NewAssistantMessage(blocks...))
			}
		case "tool":
			var resultMap map[string]any
			if err := json.Unmarshal([]byte(msg.Content), &resultMap); err != nil {
				resultMap = map[string]any{"output": msg.Content}
			}
			resultJSON, err := json.Marshal(resultMap)
			if err != nil {
				resultJSON = []byte("{\"output\": \"failed to marshal result\"}")
			}
			result = append(result, anthropic.NewUserMessage(
				anthropic.NewToolResultBlock(msg.ToolCallID, string(resultJSON), false),
			))
		}
	}
	return result
}
