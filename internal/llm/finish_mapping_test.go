package llm

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/llmwire"
)

func TestMapOpenAIFinish(t *testing.T) {
	tests := []struct {
		reason string
		want   string
	}{
		{"stop", llmwire.FinishStop},
		{"length", llmwire.FinishLength},
		{"tool_calls", llmwire.FinishToolCalls},
		{"function_call", llmwire.FinishToolCalls},
		{"content_filter", llmwire.FinishUnknown},
		{"", llmwire.FinishUnknown},
		{"some_future_reason", llmwire.FinishUnknown},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, mapOpenAIFinish(tt.reason), "finish_reason %q", tt.reason)
	}
}

func TestParseMessage_FinishReasonMapping(t *testing.T) {
	c := &openaiClient{}

	for _, tc := range []struct {
		reason string
		want   string
	}{
		{"stop", llmwire.FinishStop},
		{"length", llmwire.FinishLength},
		{"tool_calls", llmwire.FinishToolCalls},
		{"content_filter", llmwire.FinishUnknown},
	} {
		resp, err := c.parseMessage(&oaiMessage{
			Role:       roleAssistant,
			RawContent: json.RawMessage(`"partial text"`),
		}, tc.reason)
		require.NoError(t, err)
		assert.Equal(t, tc.want, resp.FinishType, "finish_reason %q", tc.reason)
		assert.Equal(t, tc.reason, resp.ProviderFinishReason)
		assert.Equal(t, "partial text", resp.Text)
	}
}

func TestParseMessage_PreservesFinishReasonWithToolCall(t *testing.T) {
	t.Parallel()

	c := &openaiClient{}
	message := &oaiMessage{
		Role:       roleAssistant,
		RawContent: json.RawMessage(`"partial text"`),
		ToolCalls: []oaiToolCall{{
			ID: "call-1",
			Function: oaiFunctionCall{
				Name:      "bash",
				Arguments: `{"command":"pwd"}`,
			},
		}},
	}

	for _, tc := range []struct {
		reason string
		want   string
	}{
		{"stop", llmwire.FinishStop},
		{"length", llmwire.FinishLength},
		{"tool_calls", llmwire.FinishToolCalls},
		{"future_reason", llmwire.FinishUnknown},
		{"", llmwire.FinishUnknown},
	} {
		resp, err := c.parseMessage(message, tc.reason)
		require.NoError(t, err)
		assert.Equal(t, tc.want, resp.FinishType, "finish_reason %q", tc.reason)
		assert.Equal(t, tc.reason, resp.ProviderFinishReason)
		require.Len(t, resp.ToolCalls, 1)
	}
}

func TestOpenAIResponseVariantsPreserveFinishEvidence(t *testing.T) {
	for _, tc := range []struct {
		reason string
		want   string
	}{
		{"stop", llmwire.FinishStop},
		{"length", llmwire.FinishLength},
		{"tool_calls", llmwire.FinishToolCalls},
		{"content_filter", llmwire.FinishUnknown},
		{"", llmwire.FinishUnknown},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			t.Parallel()

			c := &openaiClient{provider: "test", model: "test-model"}
			body, err := json.Marshal(oaiResponse{
				Choices: []oaiChoice{{
					Message:      oaiMessage{Role: roleAssistant, RawContent: json.RawMessage(`null`)},
					FinishReason: tc.reason,
				}},
				Usage: oaiUsage{PromptTokens: 3},
			})
			require.NoError(t, err)

			nonStreaming, err := c.parseResponseBody(zap.NewNop(), body, time.Now())
			require.NoError(t, err)
			assert.Empty(t, nonStreaming.Text)
			assert.Equal(t, tc.want, nonStreaming.FinishType)
			assert.Equal(t, tc.reason, nonStreaming.ProviderFinishReason)
			require.NotNil(t, nonStreaming.Usage)
			assert.Equal(t, 3, nonStreaming.Usage.PromptTokens)

			streaming, err := c.finishStreamingResponse(zap.NewNop(), &oaiStreamAggregate{
				toolCalls:    make(map[int]*oaiToolCall),
				finishReason: tc.reason,
				usage:        oaiUsage{PromptTokens: 3},
				seenChoice:   true,
			}, time.Now())
			require.NoError(t, err)
			assert.Empty(t, streaming.Text)
			assert.Equal(t, tc.want, streaming.FinishType)
			assert.Equal(t, tc.reason, streaming.ProviderFinishReason)
			require.NotNil(t, streaming.Usage)
			assert.Equal(t, 3, streaming.Usage.PromptTokens)
		})
	}
}

func TestParseResponse_StopReasonLength(t *testing.T) {
	c := &anthropicClient{}

	raw := `{"type":"text","text":"truncated answer"}`
	var block anthropic.ContentBlockUnion
	require.NoError(t, block.UnmarshalJSON([]byte(raw)))

	msg := &anthropic.Message{
		Content:    []anthropic.ContentBlockUnion{block},
		StopReason: anthropic.StopReasonMaxTokens,
	}

	resp, err := c.parseResponse(msg)
	require.NoError(t, err)
	assert.Equal(t, "truncated answer", resp.Text)
	assert.Empty(t, resp.ToolCalls)
	assert.Equal(t, llmwire.FinishLength, resp.FinishType)
	assert.Equal(t, "max_tokens", resp.ProviderFinishReason)
}

func TestParseResponse_StopReasonMappings(t *testing.T) {
	c := &anthropicClient{}

	for _, tc := range []struct {
		reason anthropic.StopReason
		want   string
	}{
		{anthropic.StopReasonEndTurn, llmwire.FinishStop},
		{anthropic.StopReasonStopSequence, llmwire.FinishStop},
		{anthropic.StopReasonMaxTokens, llmwire.FinishLength},
		{anthropic.StopReasonToolUse, llmwire.FinishToolCalls},
		{anthropic.StopReasonPauseTurn, llmwire.FinishUnknown},
		{anthropic.StopReasonRefusal, llmwire.FinishUnknown},
	} {
		resp, err := c.parseResponse(&anthropic.Message{StopReason: tc.reason})
		require.NoError(t, err)
		assert.Equal(t, tc.want, resp.FinishType, "stop_reason %q", tc.reason)
		assert.Equal(t, string(tc.reason), resp.ProviderFinishReason)
	}
}

func TestParseResponse_NilMessageIsUnknown(t *testing.T) {
	c := &anthropicClient{}
	resp, err := c.parseResponse(nil)
	require.NoError(t, err)
	assert.Equal(t, llmwire.FinishUnknown, resp.FinishType)
	assert.Empty(t, resp.ProviderFinishReason)
}

func TestParseResponse_PreservesStopReasonWithToolCall(t *testing.T) {
	t.Parallel()

	c := &anthropicClient{}
	var block anthropic.ContentBlockUnion
	require.NoError(t, block.UnmarshalJSON([]byte(
		`{"type":"tool_use","id":"call-1","name":"bash","input":{"command":"pwd"}}`,
	)))

	for _, tc := range []struct {
		reason anthropic.StopReason
		want   string
	}{
		{anthropic.StopReasonEndTurn, llmwire.FinishStop},
		{anthropic.StopReasonMaxTokens, llmwire.FinishLength},
		{anthropic.StopReasonToolUse, llmwire.FinishToolCalls},
		{anthropic.StopReason("future_reason"), llmwire.FinishUnknown},
		{anthropic.StopReason(""), llmwire.FinishUnknown},
	} {
		resp, err := c.parseResponse(&anthropic.Message{
			Content:    []anthropic.ContentBlockUnion{block},
			StopReason: tc.reason,
		})
		require.NoError(t, err)
		assert.Equal(t, tc.want, resp.FinishType)
		assert.Equal(t, string(tc.reason), resp.ProviderFinishReason)
		require.Len(t, resp.ToolCalls, 1)
	}
}
