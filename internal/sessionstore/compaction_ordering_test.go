package sessionstore

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/subagent"
)

// A background child's completion commits in its own transaction. If it lands
// after compaction read its snapshot but before ReplaceCompactedMessages
// committed, the two rows are absent from compactedIDs and carry a NULL
// position. They must survive as an intact, ordered tool_call/tool_result pair
// behind the summary — never be split, reordered, or hidden.
func TestStore_CompactionKeepsACompletionPairCommittedOutsideItsSnapshot(t *testing.T) {
	s, db, projectID := newTestStore(t)
	ctx := context.Background()

	parent, err := s.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)
	childID, err := s.CreateSubagentSession(ctx, projectID, parent.ID, parent.ID, "general", "m", "")
	require.NoError(t, err)
	seedLink(t, db, parent.ID, childID, "task-1")

	headerID, err := s.InsertMessage(ctx, parent.ID, &StoredMessage{
		Role: llmwire.RoleUser, Content: "the original task",
	})
	require.NoError(t, err)
	spawnID, err := s.InsertMessage(ctx, parent.ID, &StoredMessage{
		Role:      llmwire.RoleAssistant,
		ToolCalls: []byte(`[{"ID":"task-1","Name":"task","Arguments":"e30="}]`),
	})
	require.NoError(t, err)
	ackID, err := s.InsertMessage(ctx, parent.ID, &StoredMessage{
		Role: llmwire.RoleTool, Content: "launched", ToolCallID: "task-1", ToolName: "task",
	})
	require.NoError(t, err)

	// Compaction's snapshot is taken here: everything after the header.
	snapshot := []int64{spawnID, ackID}

	// The child completes in the window before the replacement commits.
	msgIDs, won, err := subagent.NewTransactions(db).DeliverCompletion(ctx, parent.ID, []*StoredMessage{
		{Role: llmwire.RoleAssistant, ToolCalls: []byte(`[{"ID":"ev-1","Name":"subagent_event"}]`)},
		{Role: llmwire.RoleTool, Content: "child done", ToolCallID: "ev-1", ToolName: "subagent_event"},
	}, childID, 1)
	require.NoError(t, err)
	require.True(t, won)
	require.Len(t, msgIDs, 2)

	_, err = s.ReplaceCompactedMessages(ctx, parent.ID, snapshot, []CompactionEntry{
		{ExistingID: headerID},
		{Message: &StoredMessage{Role: llmwire.RoleUser, Content: "[CONTEXT SUMMARY - previous work condensed]"}},
		{Message: &StoredMessage{Role: llmwire.RoleAssistant, Content: "ack"}},
	})
	require.NoError(t, err)

	messages, err := s.LoadActiveMessages(ctx, parent.ID)
	require.NoError(t, err)
	require.Len(t, messages, 5)

	assert.Equal(t, "the original task", messages[0].Content)
	assert.Contains(t, messages[1].Content, "[CONTEXT SUMMARY")
	assert.Equal(t, "ack", messages[2].Content)

	// The pair stayed adjacent and in producer order behind the rebuilt prefix.
	assert.Equal(t, llmwire.RoleAssistant, messages[3].Role)
	assert.Contains(t, string(messages[3].ToolCalls), "ev-1")
	assert.Equal(t, llmwire.RoleTool, messages[4].Role)
	assert.Equal(t, "ev-1", messages[4].ToolCallID)

	// The compacted originals are gone, so the summary never leaves a tool_use
	// the transcript no longer answers.
	for _, message := range messages {
		assert.NotEqual(t, spawnID, message.ID)
		assert.NotEqual(t, ackID, message.ID)
	}
}
