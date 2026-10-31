package migrate

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sessionstore"
)

func TestMigrate_56_DropsWriteOnlyColumnsPreservingRows(t *testing.T) {
	t.Parallel()

	db, err := OpenDB(t.Context(), filepath.Join(t.TempDir(), "write-only-columns.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	provider := newProvider(t, db)
	_, err = provider.UpTo(t.Context(), 55)
	require.NoError(t, err)
	draft := seedWriteOnlyColumns(t, db)
	queries := []string{
		`SELECT * FROM sessions ORDER BY id`,
		`SELECT * FROM session_outbox ORDER BY id`,
		`SELECT * FROM subagent_links ORDER BY parent_id, child_id`,
	}
	removed := [][]string{{"model_input_boundary"}, {"releases_input"}, {"delivered_msg_id", "delivered_input_id"}}
	before := make([][]map[string]sql.NullString, len(queries))
	for i, query := range queries {
		before[i] = dumpRows(t, db, query)
		for _, row := range before[i] {
			for _, column := range removed[i] {
				require.Contains(t, row, column)
				delete(row, column)
			}
		}
	}
	const indexes = `SELECT name, sql FROM sqlite_master WHERE type = 'index'
		AND tbl_name IN ('sessions', 'session_outbox', 'subagent_links') ORDER BY name`
	indexesBefore := dumpRows(t, db, indexes)
	require.Empty(t, dumpRows(t, db, `PRAGMA foreign_key_check`))
	_, err = provider.UpTo(t.Context(), 56)
	require.NoError(t, err)
	for i, query := range queries {
		assert.Equal(t, before[i], dumpRows(t, db, query), query)
	}
	assert.Equal(t, indexesBefore, dumpRows(t, db, indexes))
	assert.False(t, tableExists(t, db, "subagent_links_legacy_00056"))
	assert.Empty(t, dumpRows(t, db, `PRAGMA foreign_key_check`))
	assertWriteOnlyColumnsAbsent(t, db)
	var fingerprint string
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT fingerprint FROM session_outbox WHERE id = 30`).
		Scan(&fingerprint))
	assert.Equal(t, frozenOutputFingerprint(t, draft), fingerprint)
	_, err = provider.Up(t.Context())
	require.NoError(t, err)
	assertMigratedOutputReplay(t, db, draft)
}

func seedWriteOnlyColumns(t *testing.T, db *sql.DB) sessionstore.OutputDraft {
	t.Helper()

	_, err := db.ExecContext(t.Context(), `INSERT INTO projects(id, work_dir, name) VALUES (1, '/work', 'work');
		INSERT INTO sessions(id, project_id, agent_type, attributes, model, reasoning_level,
			iteration, status, todo_items, model_input_generation, model_input_boundary,
			context_baseline_model, context_baseline_prompt_tokens, context_baseline_message_count,
			empty_stop_streak, completion_nudge_generation)
		VALUES (1, 1, 'build', '{"manager_id":"owner"}', 'model', 'high', 3, 'active', '[{"t":"task"}]',
			7, 10, 'model', 120, 1, 2, 7);
		INSERT INTO messages(id, session_id, role, content) VALUES (10, 1, 'user', 'input');
		INSERT INTO session_inbox(id, session_id, source, raw_content, received_at)
		VALUES (20, 1, 'subagent', 'delivered result', '2026-01-01 00:00:00');
		INSERT INTO subagent_links(parent_id, child_id, task_call_id, blocking, depth, state,
			delivered_at, delivered_msg_id, created_at, result, outcome, activation_seq, delivered_input_id)
		VALUES (1, 2, 'delivered', 1, 2, 'completed', 300, 10, 100, 'done', 'completed', 3, 20),
			(1, 3, 'undelivered', 0, 1, 'error', NULL, NULL, 200, 'failed', 'error', 4, NULL)`)
	require.NoError(t, err)
	draft := sessionstore.OutputDraft{
		SessionID: 1, Type: sessionstore.OutputMessage, Content: "answer",
		Attributes: map[string]any{"source": "agent"}, SourceKey: "message:10:final", ReleasesInput: true,
	}
	_, err = db.ExecContext(
		t.Context(),
		`INSERT INTO session_outbox
		(id, session_id, type, content, attributes, source_key, fingerprint, releases_input, created_at)
		VALUES (30, 1, 'message', 'answer', '{"manager_id":"owner","source":"agent"}', ?, ?, 1, '2026-01-02 00:00:00')`,
		draft.SourceKey,
		frozenOutputFingerprint(t, draft),
	)
	require.NoError(t, err)
	return draft
}

func assertWriteOnlyColumnsAbsent(t *testing.T, db *sql.DB) {
	t.Helper()

	assert.False(t, columnExists(t, db, "sessions", "model_input_boundary"))
	assert.False(t, columnExists(t, db, "session_outbox", "releases_input"))
	assert.False(t, columnExists(t, db, "subagent_links", "delivered_msg_id"))
	assert.False(t, columnExists(t, db, "subagent_links", "delivered_input_id"))
}

func assertMigratedOutputReplay(t *testing.T, db *sql.DB, draft sessionstore.OutputDraft) {
	t.Helper()

	store := sessionstore.NewStore(db)
	output, err := store.EnqueueOutput(t.Context(), draft)
	require.NoError(t, err)
	require.NotNil(t, output)
	assert.True(t, output.Existing)
	assert.Equal(t, int64(30), output.OutputID)
	draft.ReleasesInput = false
	replay, err := store.EnqueueOutput(t.Context(), draft)
	require.NoError(t, err)
	assert.True(t, replay.Existing)
	assert.Equal(t, output.OutputID, replay.OutputID)
	assert.Equal(t, 1, countRows(t, db, "session_outbox", "1 = 1"))
}
