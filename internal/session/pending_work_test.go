package session

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pilat/coagent/internal/llmwire"
)

func asst(text string, calls ...llmwire.ToolCall) llmwire.Message {
	return llmwire.Message{Role: llmwire.RoleAssistant, Content: text, ToolCalls: calls}
}

func call(id, name string) llmwire.ToolCall { return llmwire.ToolCall{ID: id, Name: name} }

func toolRes(id string) llmwire.Message {
	return llmwire.Message{Role: llmwire.RoleTool, ToolCallID: id}
}

func usr(text string) llmwire.Message {
	return llmwire.Message{Role: llmwire.RoleUser, Content: text}
}

// TestUnresolvedToolCalls_TurnBoundary pins the fix for the notification-flood
// incident: a tool call left dangling before a user interruption is NOT pending
// work — otherwise a completed session spins on resume, re-emitting notifications.
func TestUnresolvedToolCalls_TurnBoundary(t *testing.T) {
	tests := []struct {
		name string
		msgs []llmwire.Message
		want map[string]string
	}{
		{
			name: "interrupted mid tool call then acknowledged",
			msgs: []llmwire.Message{
				usr("do the thing"),
				asst("", call("c1", "grep")),
				usr("stop"),
				asst("ok, stopping"),
			},
			want: nil,
		},
		{
			name: "dangling call then bare user reply",
			msgs: []llmwire.Message{
				asst("", call("c1", "grep")),
				usr("actually never mind"),
			},
			want: nil,
		},
		{
			name: "genuine in-flight call is pending",
			msgs: []llmwire.Message{
				usr("go"),
				asst("", call("c1", "read")),
			},
			want: map[string]string{"c1": "read"},
		},
		{
			name: "resolved call is not pending",
			msgs: []llmwire.Message{
				asst("", call("c1", "read")),
				toolRes("c1"),
			},
			want: map[string]string{},
		},
		{
			name: "partially resolved reports only the missing one",
			msgs: []llmwire.Message{
				asst("", call("c1", "read"), call("c2", "grep")),
				toolRes("c1"),
			},
			want: map[string]string{"c2": "grep"},
		},
		{
			name: "text-only last assistant is not pending",
			msgs: []llmwire.Message{
				usr("hi"),
				asst("hello, what next?"),
			},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := unresolvedToolCalls(tt.msgs)
			if tt.want == nil {
				assert.Nil(t, got)
				return
			}

			assert.Equal(t, tt.want, got)
		})
	}
}
