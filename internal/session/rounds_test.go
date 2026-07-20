package session

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pilat/coagent/internal/llmwire"
)

func asstTools() llmwire.Message {
	return llmwire.Message{Role: llmwire.RoleAssistant, Content: "a", ToolCalls: []llmwire.ToolCall{{ID: "x"}}}
}

func asstText() llmwire.Message {
	return llmwire.Message{Role: llmwire.RoleAssistant, Content: "plain"}
}

func toolResult() llmwire.Message {
	return llmwire.Message{Role: llmwire.RoleTool, Content: "r", ToolCallID: "x"}
}
func userTurn() llmwire.Message { return llmwire.Message{Role: llmwire.RoleUser, Content: "u"} }

func TestFindMaskBoundary(t *testing.T) {
	// Three complete rounds after a leading user message.
	threeRounds := []llmwire.Message{
		userTurn(),   // 0
		asstTools(),  // 1 round 1
		toolResult(), // 2
		asstTools(),  // 3 round 2
		toolResult(), // 4
		asstTools(),  // 5 round 3
		toolResult(), // 6
	}

	// Steering user message sits between round 1 and round 2.
	steeringBetween := []llmwire.Message{
		asstTools(),  // 0 round 1
		toolResult(), // 1
		userTurn(),   // 2 steering
		asstTools(),  // 3 round 2
		toolResult(), // 4
	}

	// A plain (text-only) assistant reply sits between two rounds.
	plainBetween := []llmwire.Message{
		asstTools(),  // 0 round 1
		toolResult(), // 1
		asstText(),   // 2 plain assistant
		asstTools(),  // 3 round 2
		toolResult(), // 4
	}

	tests := []struct {
		name       string
		msgs       []llmwire.Message
		keepRounds int
		want       int
	}{
		{"empty history", nil, 3, 0},
		{"keep more rounds than exist", threeRounds, 4, 0},
		{"keep one round", threeRounds, 1, 5},
		{"keep two rounds", threeRounds, 2, 3},
		{"boundary at exact round count", threeRounds, 3, 1},
		{"steering message between rounds kept", steeringBetween, 2, 0},
		{"plain assistant between rounds kept", plainBetween, 2, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, findMaskBoundary(tt.msgs, tt.keepRounds))
		})
	}
}
