package llm

import (
	"testing"

	"github.com/pilat/coagent/internal/llmwire"
)

func TestValidateToolPairing(t *testing.T) {
	tests := []struct {
		name    string
		msgs    []llmwire.Message
		wantErr bool
	}{
		{
			name:    "empty transcript is valid",
			msgs:    nil,
			wantErr: false,
		},
		{
			name: "matched tool_use and tool_result",
			msgs: []llmwire.Message{
				{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "c1", Name: "task"}}},
				{Role: llmwire.RoleTool, ToolCallID: "c1", ToolName: "task", Content: "ok"},
			},
			wantErr: false,
		},
		{
			name: "plain assistant turn with no tools",
			msgs: []llmwire.Message{
				{Role: llmwire.RoleUser, Content: "hi"},
				{Role: llmwire.RoleAssistant, Content: "hello"},
			},
			wantErr: false,
		},
		{
			name: "unresolved tool_use (no result) is invalid",
			msgs: []llmwire.Message{
				{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "c1", Name: "task"}}},
			},
			wantErr: true,
		},
		{
			name: "orphaned tool_result is invalid",
			msgs: []llmwire.Message{
				{Role: llmwire.RoleTool, ToolCallID: "ghost", ToolName: "task", Content: "ok"},
			},
			wantErr: true,
		},
		{
			name: "assistant tool_call with empty id is invalid",
			msgs: []llmwire.Message{
				{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "", Name: "task"}}},
			},
			wantErr: true,
		},
		{
			name: "one of two tool_uses unresolved is invalid",
			msgs: []llmwire.Message{
				{
					Role:      llmwire.RoleAssistant,
					ToolCalls: []llmwire.ToolCall{{ID: "c1", Name: "task"}, {ID: "c2", Name: "task"}},
				},
				{Role: llmwire.RoleTool, ToolCallID: "c1", ToolName: "task", Content: "ok"},
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateToolPairing(tc.msgs)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}

			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}
