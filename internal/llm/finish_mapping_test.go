package llm

import (
	"encoding/json"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
		assert.Equal(t, "partial text", resp.Text)
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
	}
}

func TestParseResponse_NilMessageStops(t *testing.T) {
	c := &anthropicClient{}
	resp, err := c.parseResponse(nil)
	require.NoError(t, err)
	assert.Equal(t, llmwire.FinishStop, resp.FinishType)
}
