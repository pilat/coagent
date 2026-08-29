package migrate

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Migration 28 drops the trim-before-summary state (messages.cleared_at and the
// duplicate sessions.compaction_brief) without a data backfill: stored tool
// bodies were never deleted, so formerly cleared active results become visible
// again beside their calls, while already compacted rows stay hidden.
func TestMigrate_28_DropsLegacyCompactionState(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "legacy-compaction.db")
	db, err := OpenDB(ctx, dbPath)
	require.NoError(t, err)
	defer db.Close()

	provider := newProvider(t, db)

	// Build the migration-27 world: active cleared results, compacted cleared
	// rows, and a stored brief.
	if _, err := provider.UpTo(ctx, 27); err != nil {
		t.Fatalf("migrate to 27: %v", err)
	}

	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, work_dir, name) VALUES (1, '/tmp/p', 'p')`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `
		INSERT INTO sessions (id, project_id, model, agent_type, compaction_brief)
		VALUES (1, 1, 'm', 'build', 'old brief text')`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `
		INSERT INTO messages (id, session_id, role, content, tool_call_id, tool_name, position)
		VALUES (1, 1, 'tool', 'REAL STORED BODY', 'c1', 'read', 2)`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `
		UPDATE messages SET cleared_at = '2026-01-01 00:00:00' WHERE id = 2`)
	require.NoError(t, err)

	// A compacted row that was once cleared: it stays hidden by compacted_at.
	_, err = db.ExecContext(ctx, `
		INSERT INTO messages (session_id, role, content, tool_call_id, tool_name, compacted_at, cleared_at)
		VALUES (1, 1, 'tool', 'old body', 'c-old', 'old', '2020-01-01 00:00:00')`)
	require.NoError(t, err)

	_, err = provider.Up(ctx)
	require.NoError(t, err)

	assert.False(t, columnExists(t, db, "messages", "cleared_at"), "messages.cleared_at must be dropped")
	assert.False(t, columnExists(t, db, "sessions", "compaction_brief"), "sessions.compaction_brief must be dropped")

	rows, err := db.QueryContext(ctx, `SELECT content FROM messages WHERE session_id = ? AND compacted_at IS NULL`, 1)
	require.NoError(t, err)
	defer rows.Close()

	var contents []string

	for rows.Next() {
		var content string
		require.NoError(t, rows.Scan(&content))
		contents = append(contents, content)
	}

	require.NoError(t, rows.Err())
	require.Len(t, contents, 1, "the active cleared result survives the drop untouched")
	assert.Contains(t, contents, "REAL STORED BODY", "the formerly cleared body is visible again")
}
