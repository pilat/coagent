package session

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pilat/coagent/internal/llmwire"
)

func TestRawCutLegality(t *testing.T) {
	tests := []struct {
		name  string
		tail  []llmwire.Message
		legal bool
	}{
		{name: "empty tail", tail: nil, legal: true},
		{
			name:  "complete group",
			tail:  []llmwire.Message{compactionAssistantCall("c1", "work"), compactionToolResult("c1", "result")},
			legal: true,
		},
		{
			name: "a result stored later than its call never cuts between them",
			tail: []llmwire.Message{
				compactionAssistantCall("c1", "call"),
				compactionUserMessage("interruption"),
				compactionToolResult("c1", "late result"),
			},
			legal: false,
		},
		{
			name: "duplicate call ids across assistant rows are illegal raw",
			tail: []llmwire.Message{
				compactionAssistantCall("c1", "one"),
				compactionToolResult("c1", "one result"),
				compactionAssistantCall("c1", "two"),
				compactionToolResult("c1", "two result"),
			},
			legal: false,
		},
		{
			name: "abandoned call before a newer user row must stay in the head",
			tail: []llmwire.Message{
				compactionAssistantCall("c1", "abandoned"),
				compactionUserMessage("new instruction"),
			},
			legal: false,
		},
		{
			name:  "legacy tool rows with no id are singleton groups",
			tail:  []llmwire.Message{{Role: llmwire.RoleTool, Content: "legacy", ToolName: "read"}},
			legal: true,
		},
		{
			name:  "orphaned result needs repair",
			tail:  []llmwire.Message{compactionToolResult("c1", "orphan")},
			legal: false,
		},
		{
			name:  "missing result needs repair",
			tail:  []llmwire.Message{compactionAssistantCall("c1", "unresolved")},
			legal: false,
		},
		{
			name: "one assistant row plus all parallel results is indivisible",
			tail: []llmwire.Message{
				{
					Role:      llmwire.RoleAssistant,
					ToolCalls: []llmwire.ToolCall{{ID: "p1", Name: "read"}, {ID: "p2", Name: "read"}},
				},
				compactionToolResult("p1", "one"),
			},
			legal: false,
		},
		{
			name: "a boundary between two complete parallel groups is legal",
			tail: []llmwire.Message{
				compactionAssistantCall("p1", "one"),
				compactionToolResult("p1", "one"),
				compactionAssistantCall("p2", "two"),
				compactionToolResult("p2", "two"),
			},
			legal: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.legal, rawCutLegal(tc.tail))
		})
	}
}
