package sessionstore

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/transcript"
)

func newToolErrorStore(t *testing.T) (context.Context, *store, int64) {
	t.Helper()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "tool-error.db")
	db, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(ctx, db, dbPath))

	result, err := db.ExecContext(
		ctx, `INSERT INTO projects (work_dir, name) VALUES (?, ?)`, t.TempDir(), "test",
	)
	require.NoError(t, err)

	projectID, err := result.LastInsertId()
	require.NoError(t, err)

	s := NewStore(db)
	record, err := s.CreateSession(ctx, projectID, "glm-5.3-flash", "", map[string]any{})
	require.NoError(t, err)

	return ctx, s.(*store), record.ID
}

func toolResultRow(callID, toolName, content string, toolError bool) *transcript.Message {
	return &transcript.Message{
		Role:       "tool",
		Content:    content,
		ToolCallID: callID,
		ToolName:   toolName,
		ToolError:  toolError,
	}
}

// The typed error bit survives insert and load. Legacy rows written before the
// bit existed are covered by the migration tests; here a fresh database proves
// the codec round-trip in both directions.
func TestToolErrorBit_RoundTrip(t *testing.T) {
	ctx, s, sessionID := newToolErrorStore(t)

	id1, err := s.InsertMessage(ctx, sessionID, toolResultRow("c1", "batch", "partial output", true))
	require.NoError(t, err)
	require.NotZero(t, id1)

	id2, err := s.InsertMessage(ctx, sessionID, toolResultRow("c2", "read", "file body", false))
	require.NoError(t, err)
	require.NotZero(t, id2)

	rows, err := s.LoadActiveMessages(ctx, sessionID)
	require.NoError(t, err)

	toolRows := 0

	for _, row := range rows {
		if row.Role != "tool" {
			continue
		}

		toolRows++

		switch row.ToolCallID {
		case "c1":
			assert.True(t, row.ToolError, "the typed failure bit survives the round-trip")
			assert.Equal(t, "partial output", row.Content)
		case "c2":
			assert.False(t, row.ToolError)
			assert.Equal(t, "file body", row.Content)
		}
	}

	assert.Equal(t, 2, toolRows)
}

// The atomic result-set insert commits every row of one turn and is idempotent
// by call ID: replaying the same set neither duplicates rows nor rejects.
func TestInsertToolResultSetOnce_CommitsAndReplays(t *testing.T) {
	ctx, s, sessionID := newToolErrorStore(t)

	entries := []ToolResultEntry{
		{Message: toolResultRow("c1", "batch", "partial output", true)},
		{Message: toolResultRow("c2", "read", "file body", false)},
	}

	ids, outputs, err := s.InsertToolResultSetOnce(ctx, sessionID, entries)
	require.NoError(t, err)
	require.Len(t, ids, 2)
	require.Len(t, outputs, 2)
	assert.NotZero(t, ids[0])
	assert.NotZero(t, ids[1])

	ids2, outputs2, err := s.InsertToolResultSetOnce(ctx, sessionID, entries)
	require.NoError(t, err)
	assert.Equal(t, ids, ids2, "replaying the set returns the same row ids")
	assert.Len(t, outputs2, 2)

	rows, err := s.LoadActiveMessages(ctx, sessionID)
	require.NoError(t, err)

	toolRows := 0

	for _, row := range rows {
		if row.Role == "tool" {
			toolRows++
		}
	}

	assert.Equal(t, 2, toolRows, "the replay must not duplicate result rows")
}

// A result row that already exists with different content conflicts, matching
// the single-result insert semantics; with identical content it is a no-op.
func TestInsertToolResultSetOnce_ConflictsOnContentChange(t *testing.T) {
	ctx, s, sessionID := newToolErrorStore(t)

	entries := []ToolResultEntry{{Message: toolResultRow("c1", "read", "first body", false)}}

	_, _, err := s.InsertToolResultSetOnce(ctx, sessionID, entries)
	require.NoError(t, err)

	changed := []ToolResultEntry{{Message: toolResultRow("c1", "read", "different body", false)}}
	_, _, err = s.InsertToolResultSetOnce(ctx, sessionID, changed)
	require.ErrorIs(t, err, ErrOutputConflict)

	_, _, err = s.InsertToolResultSetOnce(ctx, sessionID, entries)
	require.NoError(t, err, "the identical set is an idempotent no-op")
}
