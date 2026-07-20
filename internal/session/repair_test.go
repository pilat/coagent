package session

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pilat/coagent/internal/llmwire"
)

func TestRepairTranscript_NoOrphans(t *testing.T) {
	msgs := []llmwire.Message{
		{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "c1", Name: "read"}}},
		{Role: llmwire.RoleTool, ToolCallID: "c1", ToolName: "read", Content: "ok"},
	}
	result := repairTranscript(msgs)
	assert.Len(t, result, 2)
}

func TestRepairTranscript_RemovesOrphans(t *testing.T) {
	msgs := []llmwire.Message{
		{Role: llmwire.RoleTool, ToolCallID: "orphan1", ToolName: "grep", Content: "stale"},
		{Role: llmwire.RoleTool, ToolCallID: "orphan2", ToolName: "read", Content: "stale"},
		{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "c1", Name: "bash"}}},
		{Role: llmwire.RoleTool, ToolCallID: "c1", ToolName: "bash", Content: "ok"},
	}
	result := repairTranscript(msgs)
	assert.Len(t, result, 2)
	assert.Equal(t, llmwire.RoleAssistant, result[0].Role)
	assert.Equal(t, "c1", result[1].ToolCallID)
}

func TestRepairTranscriptExcluding_DoesNotStubPendingCall(t *testing.T) {
	// A genuinely-pending external call (e.g. a suspended blocking task) must NOT
	// get a synthetic result — stubbing it would corrupt the resume.
	msgs := []llmwire.Message{
		{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{
			{ID: "task-1", Name: "task"},
			{ID: "read-1", Name: "read"},
		}},
		{Role: llmwire.RoleTool, ToolCallID: "read-1", ToolName: "read", Content: "ok"},
	}

	result := repairTranscriptExcluding(msgs, map[string]bool{"task-1": true})

	// read-1 keeps its result; task-1 is left unmatched (no synthetic stub).
	for _, m := range result {
		if m.Role == llmwire.RoleTool && m.ToolCallID == "task-1" {
			t.Fatalf("pending task-1 must not be stubbed, got %+v", m)
		}
	}

	assert.Len(t, result, 2, "assistant turn + read result only; pending task untouched")
}

func TestRepairTranscriptExcluding_StubsNonExcludedMissingResult(t *testing.T) {
	// Without exclusion, a missing result IS stubbed — the exclude set is the only
	// thing that suppresses it.
	msgs := []llmwire.Message{
		{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "task-1", Name: "task"}}},
	}

	result := repairTranscriptExcluding(msgs, nil)

	assert.Len(t, result, 2)
	assert.Equal(t, "task-1", result[1].ToolCallID)
	assert.Contains(t, result[1].Content, "transcript repair")
}

func TestRepairTranscript_SyntheticErrorForMissingResult(t *testing.T) {
	msgs := []llmwire.Message{
		{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{
			{ID: "c1", Name: "read"},
			{ID: "c2", Name: "grep"},
		}},
		{Role: llmwire.RoleTool, ToolCallID: "c1", ToolName: "read", Content: "file content"},
	}
	result := repairTranscript(msgs)
	assert.Len(t, result, 3)
	assert.Equal(t, "c1", result[1].ToolCallID)
	assert.Equal(t, "c2", result[2].ToolCallID)
	assert.Contains(t, result[2].Content, "transcript repair")
}

func TestRepairTranscript_ReordersResults(t *testing.T) {
	msgs := []llmwire.Message{
		{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{
			{ID: "c1", Name: "read"},
			{ID: "c2", Name: "grep"},
		}},
		{Role: llmwire.RoleTool, ToolCallID: "c2", ToolName: "grep", Content: "found"},
		{Role: llmwire.RoleTool, ToolCallID: "c1", ToolName: "read", Content: "content"},
	}
	result := repairTranscript(msgs)
	assert.Len(t, result, 3)
	assert.Equal(t, "c1", result[1].ToolCallID)
	assert.Equal(t, "c2", result[2].ToolCallID)
}

func TestRepairTranscript_SkipsSyntheticForIncompleteToolCalls(t *testing.T) {
	msgs := []llmwire.Message{
		{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{
			{ID: "c1", Name: "read"},
			{ID: "c2", Name: ""},
		}},
		{Role: llmwire.RoleTool, ToolCallID: "c1", ToolName: "read", Content: "ok"},
	}
	result := repairTranscript(msgs)
	assert.Len(t, result, 2)
	assert.Equal(t, "c1", result[1].ToolCallID)
}

func TestRepairTranscript_Empty(t *testing.T) {
	result := repairTranscript(nil)
	assert.Nil(t, result)
}
