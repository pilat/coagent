package session

import (
	"strings"

	"github.com/pilat/coagent/internal/llmwire"
)

func userTokens(tokens int) llmwire.Message {
	return llmwire.Message{Role: llmwire.RoleUser, Content: strings.Repeat("u", tokens*4)}
}

// roundTokens builds an assistant-with-tool-call plus its result, sized in tokens.
func roundTokens(id string, callTokens, resultTokens int) []llmwire.Message {
	return []llmwire.Message{
		{
			Role:      llmwire.RoleAssistant,
			Content:   strings.Repeat("a", callTokens*4),
			ToolCalls: []llmwire.ToolCall{{ID: id, Name: "read"}},
		},
		{Role: llmwire.RoleTool, Content: strings.Repeat("t", resultTokens*4), ToolCallID: id, ToolName: "read"},
	}
}
