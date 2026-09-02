package llm

import (
	"encoding/json"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
)

// The provider protocol for a native multiple-call turn: one assistant message
// of tool_use blocks followed by ONE user message whose tool_result blocks are
// ordered by the assistant call list, with is_error set only on the failed call.
func TestAnthropicConvertMessages_ToolRunGroupsAndOrdersResults(t *testing.T) {
	c := &anthropicClient{model: "claude-3-5-haiku-latest"}

	assistant := llmwire.Message{
		Role: llmwire.RoleAssistant,
		ToolCalls: []llmwire.ToolCall{
			{ID: "call-b", Name: "grep", Arguments: json.RawMessage(`{}`)},
			{ID: "call-a", Name: "read", Arguments: json.RawMessage(`{}`)},
			{ID: "call-c", Name: "edit", Arguments: json.RawMessage(`{}`)},
		},
	}

	// Results arrive out of call order; the failed one carries the typed bit.
	messages := []llmwire.Message{
		{Role: llmwire.RoleUser, Content: "go"},
		assistant,
		{
			Role:       llmwire.RoleTool,
			ToolCallID: "call-c",
			ToolName:   "edit",
			Content:    `{"output":"skipped"}`,
			ToolError:  true,
		},
		{Role: llmwire.RoleTool, ToolCallID: "call-b", ToolName: "grep", Content: `{"output":"grep out"}`},
		{Role: llmwire.RoleTool, ToolCallID: "call-a", ToolName: "read", Content: `{"output":"read out"}`},
	}

	result := c.convertMessages(messages)
	require.Len(t, result, 3, "user + assistant + ONE grouped tool-result message")

	assert.Equal(t, "go", blockText(t, result[0]))

	assistantParam := result[1]
	require.Len(t, assistantParam.Content, 3, "one tool_use block per call")

	toolUses := make([]string, 0, 3)

	for _, block := range assistantParam.Content {
		if block.OfToolUse != nil {
			toolUses = append(toolUses, block.OfToolUse.ID)
		}
	}

	assert.Equal(t, []string{"call-b", "call-a", "call-c"}, toolUses, "assistant keeps the call list verbatim")

	grouped := result[2]
	require.Len(t, grouped.Content, 3, "all three results merge into one user message")

	type toolResult struct {
		id      string
		isError bool
	}

	var got []toolResult

	for _, block := range grouped.Content {
		tr := block.OfToolResult
		require.NotNil(t, tr, "the grouped message carries only tool_result blocks")
		got = append(got, toolResult{id: tr.ToolUseID, isError: tr.IsError.Value})
	}

	assert.Equal(t, []toolResult{
		{"call-b", false},
		{"call-a", false},
		{"call-c", true},
	}, got, "blocks follow the assistant call order; only the failed call is an error")
}

func blockText(t *testing.T, param anthropic.MessageParam) string {
	t.Helper()

	for _, block := range param.Content {
		if block.OfText != nil {
			return block.OfText.Text
		}
	}

	return ""
}
