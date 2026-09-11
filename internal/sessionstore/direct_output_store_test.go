package sessionstore

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/transcript"
)

func TestDirectOutputStore_CommitsToolResultAndOrderedOutputs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	root, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "cli"})
	require.NoError(t, err)
	message := &transcript.Message{
		Role: "tool", Content: "model result", ToolCallID: "call-1",
		ToolName: "example", CreatedAt: time.Now().UTC(),
	}

	messageID, outputs, err := store.InsertToolResultWithDirectOutput(
		ctx, root.ID, message, []string{"first", "second"},
	)
	require.NoError(t, err)
	assert.Positive(t, messageID)
	require.Len(t, outputs, 2)
	assert.Less(t, outputs[0].OutputID, outputs[1].OutputID)

	replayedID, replayed, err := store.InsertToolResultWithDirectOutput(
		ctx, root.ID, message, []string{"first", "second"},
	)
	require.NoError(t, err)
	assert.Equal(t, messageID, replayedID)
	assert.True(t, replayed[0].Existing)

	var messages, outbox int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages
		WHERE session_id = ? AND tool_call_id = 'call-1'`, root.ID).Scan(&messages))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_outbox
		WHERE session_id = ? AND source_key LIKE 'tool:call-1:direct:%'`, root.ID).Scan(&outbox))
	assert.Equal(t, 1, messages)
	assert.Equal(t, 2, outbox)
}

func TestDirectOutputStore_OwnerlessRootKeepsOutputInternal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	root, err := store.CreateSession(ctx, projectID, "model", "", nil)
	require.NoError(t, err)
	message := &transcript.Message{
		Role: "tool", Content: "model result", ToolCallID: "call-1",
		ToolName: "example", CreatedAt: time.Now().UTC(),
	}

	_, outputs, err := store.InsertToolResultWithDirectOutput(ctx, root.ID, message, []string{"private"})
	require.NoError(t, err)
	assert.Empty(t, outputs)
	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_outbox
		WHERE session_id = ?`, root.ID).Scan(&count))
	assert.Zero(t, count)
}

func TestDirectOutputStore_ZeroTimestampUsesUTCTransactionTime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	root, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "cli"})
	require.NoError(t, err)

	message := &transcript.Message{
		Role: "tool", Content: "model result", ToolCallID: "plain-call", ToolName: "example",
	}
	messageID, outputs, err := store.InsertToolResultWithDirectOutput(ctx, root.ID, message, nil)
	require.NoError(t, err)
	assert.Empty(t, outputs)

	var createdAt time.Time
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT created_at FROM messages WHERE id = ?`, messageID,
	).Scan(&createdAt))
	assert.False(t, createdAt.IsZero())
	assert.Equal(t, time.UTC, createdAt.Location())
}

func TestDirectOutputStore_ZeroTimestampWithDirectOutputUsesUTCTransactionTime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	root, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "cli"})
	require.NoError(t, err)

	message := &transcript.Message{
		Role: "tool", Content: "model result", ToolCallID: "direct-zero-call", ToolName: "example",
	}
	messageID, outputs, err := store.InsertToolResultWithDirectOutput(
		ctx, root.ID, message, []string{"direct"},
	)
	require.NoError(t, err)
	require.Len(t, outputs, 1)

	var createdAt time.Time
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT created_at FROM messages WHERE id = ?`, messageID,
	).Scan(&createdAt))
	assert.False(t, createdAt.IsZero())
	assert.Equal(t, time.UTC, createdAt.Location())
}

func TestDirectOutputStore_ExplicitTimestampSurvivesReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	root, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "cli"})
	require.NoError(t, err)

	explicit := time.Date(2025, time.February, 3, 4, 5, 6, 789, time.UTC)
	message := &transcript.Message{
		Role: "tool", Content: "model result", ToolCallID: "direct-call",
		ToolName: "example", CreatedAt: explicit,
	}
	messageID, outputs, err := store.InsertToolResultWithDirectOutput(ctx, root.ID, message, []string{"direct"})
	require.NoError(t, err)
	require.Len(t, outputs, 1)

	replay := *message
	replay.CreatedAt = explicit.Add(time.Hour)
	replayedID, replayedOutputs, err := store.InsertToolResultWithDirectOutput(
		ctx, root.ID, &replay, []string{"direct"},
	)
	require.NoError(t, err)
	assert.Equal(t, messageID, replayedID)
	require.Len(t, replayedOutputs, 1)
	assert.True(t, replayedOutputs[0].Existing)

	var createdAt time.Time
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT created_at FROM messages WHERE id = ?`, messageID,
	).Scan(&createdAt))
	assert.True(t, createdAt.Equal(explicit), "replay must not replace the original explicit timestamp")

	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages
		WHERE session_id = ? AND tool_call_id = ?`, root.ID, message.ToolCallID).Scan(&count))
	assert.Equal(t, 1, count)
}

func TestDirectOutputStore_ResultSetSharesUTCTransactionTime(t *testing.T) {
	t.Parallel()
	ctx, store, sessionID := newToolErrorStore(t)

	entries := []ToolResultEntry{
		{Message: toolResultRow("staged-1", "first", "one", false)},
		{Message: toolResultRow("staged-2", "second", "two", false)},
	}
	ids, _, err := store.InsertToolResultSetOnce(ctx, sessionID, entries)
	require.NoError(t, err)
	require.Len(t, ids, 2)

	createdAt := make([]time.Time, len(ids))
	for i, id := range ids {
		require.NoError(t, store.db.QueryRowContext(ctx,
			`SELECT created_at FROM messages WHERE id = ?`, id,
		).Scan(&createdAt[i]))
		assert.False(t, createdAt[i].IsZero())
		assert.Equal(t, time.UTC, createdAt[i].Location())
	}
	assert.Equal(t, createdAt[0], createdAt[1])
}
