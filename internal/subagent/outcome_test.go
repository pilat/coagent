package subagent

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pilat/coagent/internal/transcript"
)

func TestOutcomeTranscriptPredicates(t *testing.T) {
	tests := []struct {
		name     string
		messages []*transcript.Message
		text     string
		final    bool
		calls    bool
	}{
		{name: "empty transcript"},
		{
			name: "assistant text",
			messages: []*transcript.Message{
				{Role: "user", Content: "hello"},
				{Role: "assistant", Content: "how can I help?"},
			},
			text: "how can I help?", final: true,
		},
		{
			name: "assistant with tool calls",
			messages: []*transcript.Message{
				{Role: "user", Content: "run something"},
				{Role: "assistant", Content: "sure", ToolCalls: []byte(`[{"id":"tc1","name":"bash"}]`)},
			},
			calls: true,
		},
		{
			name: "user follow-up fences the final answer",
			messages: []*transcript.Message{
				{Role: "assistant", Content: "previous reply"},
				{Role: "user", Content: "follow-up question"},
			},
			text: "previous reply",
		},
		{
			name: "assistant text followed by a tool result",
			messages: []*transcript.Message{
				{Role: "assistant", Content: "answer"},
				{Role: "tool", ToolCallID: "tc1", Content: "done"},
			},
			text: "answer", final: true,
		},
		{
			name:     "assistant whitespace is not a final answer",
			messages: []*transcript.Message{{Role: "assistant", Content: " \n\t"}},
		},
		{
			name:     "truncated assistant is not a final answer",
			messages: []*transcript.Message{{Role: "assistant", Content: "partial", FinishType: "length"}},
		},
		{
			name: "tool result preserves the assistant calls",
			messages: []*transcript.Message{
				{Role: "assistant", ToolCalls: []byte(`[{"id":"tc1","name":"bash"}]`)},
				{Role: "tool", ToolCallID: "tc1", Content: "done"},
			},
			calls: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.text, lastAssistantText(tt.messages))
			assert.Equal(t, tt.final, lastMessageIsFinalAnswer(tt.messages))
			assert.Equal(t, tt.calls, lastAssistantHasCalls(tt.messages))
		})
	}
}
