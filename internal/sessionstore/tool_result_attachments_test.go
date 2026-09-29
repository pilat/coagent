package sessionstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/transcript"
)

func TestInsertToolResultSetOnce_PreservesAttachmentsAcrossReloadAndReplay(t *testing.T) {
	ctx, store, sessionID := newToolErrorStore(t)
	image := toolResultRow("image", "read", "image result", false)
	image.Attachments = []byte(
		`[{"path":"/images/a.png","read_root":"/images","read_root_id":"root-1","mime":"image/png","size":42,"width":10,"height":20}]`,
	)
	imageEntry, err := toolResultEntry(ctx, store, sessionID, image, nil)
	require.NoError(t, err)
	failedEntry, err := toolResultEntry(ctx, store, sessionID, toolResultRow("failed", "read", "failed read", true), nil)
	require.NoError(t, err)
	entries := []ToolResultEntry{
		imageEntry,
		failedEntry,
	}
	ids, _, err := store.InsertToolResultSetOnce(ctx, sessionID, entries)
	require.NoError(t, err)
	require.Len(t, ids, 2)

	reloaded := NewStore(store.db)
	rows, err := reloaded.LoadActiveMessages(ctx, sessionID)
	require.NoError(t, err)
	toolRows := make([]*transcript.Message, 0, 2)
	for _, row := range rows {
		if row.Role == "tool" {
			toolRows = append(toolRows, row)
		}
	}
	require.Len(t, toolRows, 2)
	assert.Equal(t, ids[0], toolRows[0].ID)
	assert.Equal(t, image.Attachments, toolRows[0].Attachments)
	assert.True(t, toolRows[1].ToolError)

	seedPendingCheck(t, store.db, sessionID)
	replayed, _, err := reloaded.InsertToolResultSetOnce(ctx, sessionID, entries)
	require.NoError(t, err)
	assert.Equal(t, ids, replayed)
	candidate, _, streak := readCompletionState(t, store.db, sessionID)
	assert.Equal(t, int64(9000), candidate.Int64)
	assert.Equal(t, 2, streak)
	var count int
	require.NoError(t, store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE session_id = ? AND role = 'tool'`, sessionID).Scan(&count))
	assert.Equal(t, 2, count)
}

func TestInsertToolResultSetOnce_AttachmentReplayIdentity(t *testing.T) {
	image := []byte(`[{"path":"/images/a.png","mime":"image/png","size":42}]`)
	for _, tt := range []struct {
		name     string
		initial  []byte
		replay   []byte
		conflict bool
	}{
		{name: "absent and empty", replay: []byte{}},
		{name: "absent and null", replay: []byte(`null`)},
		{name: "absent and empty array", replay: []byte(`[]`)},
		{name: "empty array and absent", initial: []byte(`[]`)},
		{name: "null and absent", initial: []byte(`null`)},
		{name: "added", replay: image, conflict: true},
		{name: "removed", initial: image, conflict: true},
		{
			name: "changed", initial: image,
			replay: []byte(`[{"path":"/images/b.png","mime":"image/png","size":42}]`), conflict: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, store, sessionID := newToolErrorStore(t)
			message := toolResultRow("image", "read", "image result", false)
			message.Attachments = tt.initial
			entry, err := toolResultEntry(ctx, store, sessionID, message, nil)
			require.NoError(t, err)
			ids, _, err := store.InsertToolResultSetOnce(ctx, sessionID, []ToolResultEntry{entry})
			require.NoError(t, err)

			replay := *message
			replay.Attachments = tt.replay
			replayEntry := entry
			replayEntry.Message = &replay
			replayed, _, err := store.InsertToolResultSetOnce(ctx, sessionID, []ToolResultEntry{replayEntry})
			if tt.conflict {
				require.ErrorIs(t, err, ErrOutputConflict)
			} else {
				require.NoError(t, err)
				assert.Equal(t, ids, replayed)
			}
		})
	}
}

func TestInsertToolResultSetOnce_AttachmentConflictRollsBackFreshSettlement(t *testing.T) {
	ctx, store, sessionID := newToolErrorStore(t)
	image := toolResultRow("image", "read", "image result", false)
	image.Attachments = []byte(`[{"path":"/images/a.png","mime":"image/png","size":42}]`)
	imageEntry, err := toolResultEntry(ctx, store, sessionID, image, nil)
	require.NoError(t, err)
	_, _, err = store.InsertToolResultSetOnce(ctx, sessionID, []ToolResultEntry{imageEntry})
	require.NoError(t, err)
	seedPendingCheck(t, store.db, sessionID)

	conflict := *image
	conflict.Attachments = []byte(`[{"path":"/images/b.png","mime":"image/png","size":42}]`)
	conflictEntry := imageEntry
	conflictEntry.Message = &conflict
	freshEntry, err := toolResultEntry(ctx, store, sessionID, toolResultRow("fresh", "read", "fresh result", false), nil)
	require.NoError(t, err)
	_, _, err = store.InsertToolResultSetOnce(ctx, sessionID, []ToolResultEntry{
		freshEntry,
		conflictEntry,
	})
	require.ErrorIs(t, err, ErrOutputConflict)

	candidate, _, streak := readCompletionState(t, store.db, sessionID)
	assert.Equal(t, int64(9000), candidate.Int64, "failed settlement preserves the newer check")
	assert.Equal(t, 2, streak)
	var count int
	require.NoError(t, store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE session_id = ? AND tool_call_id = 'fresh'`, sessionID).Scan(&count))
	assert.Zero(t, count, "the earlier fresh result rolls back with the conflicting replay")
}
