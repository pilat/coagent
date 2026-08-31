package sessionstore

import (
	"context"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/transcript"
)

func TestStore_CompleteCompactionInputCommitsReplacementAndOutcomeTogether(t *testing.T) {
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	record, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "cli"})
	require.NoError(t, err)
	input, err := store.EnqueueInput(ctx, record.ID, InputSourceUser, "/compact focus")
	require.NoError(t, err)
	headerID, err := store.InsertMessage(ctx, record.ID, &transcript.Message{Role: "user", Content: "task"})
	require.NoError(t, err)
	oldID, err := store.InsertMessage(ctx, record.ID, &transcript.Message{Role: "assistant", Content: "long work"})
	require.NoError(t, err)

	compactionStore, ok := store.(CompactionCommandStore)
	require.True(t, ok)
	ids, output, err := compactionStore.CompleteCompactionInput(
		ctx,
		input.ID,
		record.ID,
		[]int64{oldID},
		[]CompactionEntry{
			{ExistingID: headerID},
			{Message: &transcript.Message{Role: "user", Content: "[CONTEXT SUMMARY] brief"}},
		},
		"✅ Context compacted",
	)
	require.NoError(t, err)
	require.Len(t, ids, 2)
	require.NotNil(t, output)

	var inputState, sourceKey, content string
	require.NoError(
		t,
		db.QueryRowContext(ctx, `SELECT state FROM session_inbox WHERE id = ?`, input.ID).Scan(&inputState),
	)
	require.NoError(
		t,
		db.QueryRowContext(ctx, `SELECT source_key, content FROM session_outbox WHERE id = ?`, output.OutputID).
			Scan(&sourceKey, &content),
	)
	assert.Equal(t, InputStateHandled, InputState(inputState))
	assert.Equal(t, "input:"+strconv.FormatInt(input.ID, 10)+":compact:succeeded", sourceKey)
	assert.Equal(t, "✅ Context compacted", content)

	var compacted bool
	require.NoError(
		t,
		db.QueryRowContext(ctx, `SELECT compacted_at IS NOT NULL FROM messages WHERE id = ?`, oldID).Scan(&compacted),
	)
	assert.True(t, compacted)
}
