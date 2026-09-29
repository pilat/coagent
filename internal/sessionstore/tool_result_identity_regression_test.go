package sessionstore

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/transcript"
)

func TestInsertToolResultSetOnce_SameCallIDAfterCompactionUsesInvocationIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	session, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "telegram-test"})
	require.NoError(t, err)

	assistantA, err := store.InsertMessage(ctx, session.ID, &transcript.Message{
		Role:      llmwire.RoleAssistant,
		ToolCalls: []byte(`[{"ID":"reused-call","Name":"read","Arguments":"e30="}]`),
	})
	require.NoError(t, err)
	entryA := ToolResultEntry{
		Message: toolResultRow("reused-call", "read", "old result", false),
		CallRef: CallRef{AssistantMessageID: assistantA, Index: 0},
	}
	idsA, _, err := store.InsertToolResultSetOnce(ctx, session.ID, []ToolResultEntry{entryA})
	require.NoError(t, err)
	require.Len(t, idsA, 1)

	_, err = store.ReplaceCompactedMessages(ctx, session.ID, []int64{assistantA, idsA[0]}, []CompactionEntry{
		{Message: &transcript.Message{Role: llmwire.RoleUser, Content: "[summary]"}},
	})
	require.NoError(t, err)

	assistantB, err := store.InsertMessage(ctx, session.ID, &transcript.Message{
		Role:      llmwire.RoleAssistant,
		ToolCalls: []byte(`[{"ID":"reused-call","Name":"read","Arguments":"e30="}]`),
	})
	require.NoError(t, err)
	entryB := ToolResultEntry{
		Message: toolResultRow("reused-call", "read", "new result", false),
		CallRef: CallRef{AssistantMessageID: assistantB, Index: 0},
	}
	idsB, _, err := store.InsertToolResultSetOnce(ctx, session.ID, []ToolResultEntry{entryB})
	require.NoError(t, err)
	require.Len(t, idsB, 1)
	assert.NotEqual(t, idsA[0], idsB[0], "the later invocation must get a fresh durable result row")

	replayed, _, err := store.InsertToolResultSetOnce(ctx, session.ID, []ToolResultEntry{entryB})
	require.NoError(t, err)
	assert.Equal(t, idsB, replayed, "retrying the later invocation must return its own result row")

	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages
		WHERE session_id = ? AND role = 'tool' AND tool_call_id = 'reused-call'`, session.ID).Scan(&count))
	assert.Equal(t, 2, count, "compacted history and the active result remain distinct durable invocations")
}

func TestInsertToolResultSetOnce_DirectOutputIdentityIncludesInvocation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	session, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "telegram-test"})
	require.NoError(t, err)

	entries := make([]ToolResultEntry, 2)
	for i, body := range []string{"first result", "second result"} {
		assistantID, insertErr := store.InsertMessage(ctx, session.ID, &transcript.Message{
			Role:      llmwire.RoleAssistant,
			ToolCalls: []byte(`[{"ID":"reused-call","Name":"read","Arguments":"e30="}]`),
		})
		require.NoError(t, insertErr)
		entries[i] = ToolResultEntry{
			Message:        toolResultRow("reused-call", "read", body, false),
			DirectMessages: []string{"direct output " + body},
			CallRef:        CallRef{AssistantMessageID: assistantID, Index: 0},
		}
	}

	ids, outputs, err := store.InsertToolResultSetOnce(ctx, session.ID, entries)
	require.NoError(t, err)
	require.Len(t, ids, 2)
	require.Len(t, outputs, 2)
	require.Len(t, outputs[0], 1)
	require.Len(t, outputs[1], 1)
	assert.NotEqual(t, outputs[0][0].OutputID, outputs[1][0].OutputID,
		"distinct invocations with the same provider call ID must not collide in the outbox")

	var keys int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT source_key) FROM session_outbox
		WHERE session_id = ? AND type = 'message_persistent'`, session.ID).Scan(&keys))
	assert.Equal(t, 2, keys)

	replayed, replayOutputs, err := store.InsertToolResultSetOnce(ctx, session.ID, entries)
	require.NoError(t, err)
	assert.Equal(t, ids, replayed)
	assert.True(t, replayOutputs[0][0].Existing)
	assert.True(t, replayOutputs[1][0].Existing)
}
