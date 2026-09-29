package sessioncalls

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestLatestCallRefSelectsCurrentInvocationAfterIDReuse(t *testing.T) {
	messages := []llmwire.Message{
		{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "same", Name: "read"}}},
		{Role: llmwire.RoleTool, ToolCallID: "same", ToolName: "read"},
		{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "other", Name: "read"}, {ID: "same", Name: "read"}}},
	}
	ref, err := LatestCallRef(messages, []int64{10, 11, 20}, "same", "read")
	require.NoError(t, err)
	require.Equal(t, sessionstore.CallRef{AssistantMessageID: 20, Index: 1}, ref)
}
