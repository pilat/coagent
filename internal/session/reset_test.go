package session

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/todo"
)

func newResetTestSvc(store *compactionRecordingStore) *svc {
	return &svc{
		id:           1,
		agentsMD:     "PROJECT RULES",
		ms:           newMessageStore(store, 1),
		loopDetector: newLoopDetector(),
		todoStore:    todo.New(),
		store:        store,
		prompt:       newPromptBuilder(testPrompt, "", ""),
	}
}

func TestResetContextAndInjectOnce_StartsBlankSlate(t *testing.T) {
	ctx := context.Background()
	store := &compactionRecordingStore{nextID: 1}
	s := newResetTestSvc(store)

	for _, m := range []llmwire.Message{
		{Role: llmwire.RoleUser, Content: "old task"},
		{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "c1", Name: "read"}}},
		{Role: llmwire.RoleTool, Content: "old result", ToolCallID: "c1", ToolName: "read"},
	} {
		s.ms.mu.Lock()
		err := s.ms.appendMessageLocked(ctx, &m)
		s.ms.mu.Unlock()
		require.NoError(t, err)
	}

	s.todoStore.Add("old todo", todo.PriorityMedium)

	applied, err := s.ResetContextAndInjectOnce(ctx, "reset:fresh:1", "do the fresh job")
	require.NoError(t, err)
	require.True(t, applied)

	// In-memory view is exactly the reopened turn: AGENTS.md header + the new task.
	msgs := s.ms.getMessages()
	require.Len(t, msgs, 2)
	assert.Equal(t, agentsMDMessagePrefix+"PROJECT RULES", msgs[0].Content)
	assert.Contains(t, msgs[1].Content, "do the fresh job")
	assert.NotContains(t, msgs[1].Content, "old task")

	// Derived context is gone.
	assert.Empty(t, s.todoStore.List())

	// The persisted projection agrees: a reload sees only the fresh two rows —
	// the old transcript is hidden (compacted), not deleted.
	reloaded := newMessageStore(store, 1)
	require.NoError(t, reloaded.reloadMessages(ctx))
	rl := reloaded.getMessages()
	require.Len(t, rl, 2)
	assert.Equal(t, agentsMDMessagePrefix+"PROJECT RULES", rl[0].Content)
	assert.Contains(t, rl[1].Content, "do the fresh job")
}
