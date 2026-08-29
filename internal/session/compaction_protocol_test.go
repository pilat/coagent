package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
)

func TestSerializeCanonicalGolden(t *testing.T) {
	msgs := []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		{Role: llmwire.RoleUser, Content: "hello"},
		{
			Role:      llmwire.RoleAssistant,
			ToolCalls: []llmwire.ToolCall{{ID: "c1", Name: "read", Arguments: []byte(`{"path":"a.go"}`)}},
		},
		{Role: llmwire.RoleTool, ToolCallID: "c1", ToolName: "read", Content: "file body"},
	}

	want := `{"role":"system","content":{"encoding":"utf8","data":"sys"},"tool_call_id":"","tool_name":"","tool_calls":[],"attachments":[]}
{"role":"user","content":{"encoding":"utf8","data":"hello"},"tool_call_id":"","tool_name":"","tool_calls":[],"attachments":[]}
{"role":"assistant","content":{"encoding":"utf8","data":""},"tool_call_id":"","tool_name":"","tool_calls":[{"id":"c1","name":"read","arguments":{"encoding":"utf8","data":"{\"path\":\"a.go\"}"}}],"attachments":[]}
{"role":"tool","content":{"encoding":"utf8","data":"file body"},"tool_call_id":"c1","tool_name":"read","tool_calls":[],"attachments":[]}
`

	got, err := serializeCanonical(msgs)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestSerializeCanonicalDeterministic(t *testing.T) {
	msgs := []llmwire.Message{
		{Role: llmwire.RoleUser, Content: "hello"},
		{
			Role:      llmwire.RoleAssistant,
			ToolCalls: []llmwire.ToolCall{{ID: "c1", Name: "read", Arguments: []byte(`{"path":"a.go"}`)}},
		},
		{Role: llmwire.RoleTool, ToolCallID: "c1", ToolName: "read", Content: "body"},
		{
			Role:    llmwire.RoleTool,
			Content: "with attachment",
			Images:  []llmwire.ImageRef{{Path: "/tmp/p.png", Mime: "image/png", Size: 42}},
		},
	}

	out, err := serializeCanonical(msgs)
	require.NoError(t, err)
	assert.Contains(t, out, `"path":"/tmp/p.png","mime":"image/png","size":42`)
	assert.NotContains(t, out, `"reasoning`, "opaque reasoning payloads and usage never enter the projection")
}

func TestSerializeCanonicalInvalidUTF8(t *testing.T) {
	out, err := serializeCanonical([]llmwire.Message{{Role: llmwire.RoleUser, Content: "ok \xff"}})
	require.NoError(t, err)
	assert.Contains(t, out, `"encoding":"base64"`)
}

func TestSerializeCanonicalMalformedArguments(t *testing.T) {
	msgs := []llmwire.Message{
		{
			Role:      llmwire.RoleAssistant,
			ToolCalls: []llmwire.ToolCall{{ID: "a", Name: "b", Arguments: []byte{0xff, 0xfe}}},
		},
	}

	out, err := serializeCanonical(msgs)
	require.NoError(t, err)
	assert.Contains(t, out, `"arguments":{"encoding":"base64"`)
}

func TestSerializeCanonicalTrailingNewline(t *testing.T) {
	out, err := serializeCanonical([]llmwire.Message{
		{Role: llmwire.RoleUser, Content: "x"},
		{Role: llmwire.RoleUser, Content: "y"},
	})
	require.NoError(t, err)

	assert.Equal(t, 2, strings.Count(out, "\n"), "one record per line, newline after every record")
}

func TestSerializeCanonicalSkipsNothing(t *testing.T) {
	// Every record emits all six fields, empty values included.
	out, err := serializeCanonical([]llmwire.Message{{Role: llmwire.RoleUser, Content: "x"}})
	require.NoError(t, err)

	want := `{"role":"user","content":{"encoding":"utf8","data":"x"},"tool_call_id":"","tool_name":"","tool_calls":[],"attachments":[]}` + "\n"
	assert.Equal(t, want, out)
}

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
