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
