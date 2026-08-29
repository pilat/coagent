package migrate

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The schema default was the bug's accomplice: an INSERT that forgets agent_type
// must fail loudly instead of silently minting a 'general' session.
func TestMigrate_SessionsAgentTypeHasNoDefaultOnFreshDB(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "fresh22.db")
	db, err := OpenDB(ctx, dbPath)
	require.NoError(t, err)
	defer db.Close()

	require.NoError(t, Run(ctx, db, dbPath))

	notNull, dflt, found := columnConstraint(t, db, "sessions", "agent_type")
	require.True(t, found, "sessions.agent_type must exist")
	assert.True(t, notNull, "sessions.agent_type must be NOT NULL")
	assert.False(t, dflt.Valid, "sessions.agent_type must carry no default, got %q", dflt.String)

	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, work_dir, name) VALUES (1, '/tmp/p', 'p')`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `INSERT INTO sessions (id, project_id, model) VALUES (1, 1, 'm')`)
	require.Error(t, err, "an INSERT omitting agent_type must fail")
	assert.Contains(t, strings.ToLower(err.Error()), "not null")

	_, err = db.ExecContext(ctx,
		`INSERT INTO sessions (id, project_id, model, agent_type) VALUES (1, 1, 'm', 'build')`)
	require.NoError(t, err, "a complete INSERT still works")

	var sqlText string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='sessions'`).Scan(&sqlText))
	assert.NotContains(t, sqlText, "'general'", "the dead default must be gone from the schema")
}

// The table rebuild must be a pure schema change: every row, every index and
// every foreign key referencing sessions survives it untouched.
func TestMigrate_SessionsAgentTypeRebuildPreservesExistingDB(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "v21.db")
	db, err := OpenDB(ctx, dbPath)
	require.NoError(t, err)
	defer db.Close()

	provider := newProvider(t, db)
	_, err = provider.UpTo(ctx, 21)
	require.NoError(t, err)

	seedSessionsAtV21(t, db)

	const sessionIndexes = `SELECT name, COALESCE(sql, '') FROM sqlite_master
		WHERE type='index' AND tbl_name='sessions' ORDER BY name`

	rowsBefore := dumpRows(t, db, `SELECT * FROM sessions ORDER BY id`)
	// Migration 28 later drops compaction_brief; the rebuild comparison uses the
	// columns both sides have after the full run.
	for i := range rowsBefore {
		delete(rowsBefore[i], "compaction_brief")
	}
	indexesBefore := dumpRows(t, db, sessionIndexes)
	require.Len(t, indexesBefore, 1, "idx_sessions_project_id is the only index on sessions")

	_, err = provider.Up(ctx)
	require.NoError(t, err)

	rowsAfter := dumpRows(t, db, `SELECT * FROM sessions ORDER BY id`)
	require.Len(t, rowsAfter, len(rowsBefore))

	// Legacy NULLs get 00021's rule; every other row is byte-for-byte identical.
	for i := range rowsBefore {
		if rowsBefore[i]["id"].String == "1" {
			assert.Equal(t, rowsBefore[i]["updated_at"].String, rowsAfter[i]["episode_started_at"].String)
			rowsBefore[i]["episode_started_at"] = rowsAfter[i]["episode_started_at"]
		} else {
			rowsBefore[i]["episode_started_at"] = sql.NullString{}
		}
		// 00028 backfills generation only for sessions with transcript history.
		if rowsBefore[i]["id"].String == "1" {
			assert.Equal(t, "1", rowsAfter[i]["model_input_generation"].String)
			assert.Equal(t, "1", rowsAfter[i]["model_input_boundary"].String)
		} else {
			assert.Equal(t, "0", rowsAfter[i]["model_input_generation"].String)
			assert.False(t, rowsAfter[i]["model_input_boundary"].Valid)
		}
		rowsBefore[i]["model_input_generation"] = rowsAfter[i]["model_input_generation"]
		rowsBefore[i]["model_input_boundary"] = rowsAfter[i]["model_input_boundary"]
		switch rowsBefore[i]["id"].String {
		case "4":
			assert.False(t, rowsBefore[i]["agent_type"].Valid, "row 4 was the legacy NULL root")
			assert.Equal(t, "build", rowsAfter[i]["agent_type"].String)
			rowsBefore[i]["agent_type"] = rowsAfter[i]["agent_type"]
		case "5":
			assert.False(t, rowsBefore[i]["agent_type"].Valid, "row 5 was the legacy NULL subagent")
			assert.Equal(t, "general", rowsAfter[i]["agent_type"].String)
			rowsBefore[i]["agent_type"] = rowsAfter[i]["agent_type"]
		}

		assert.Equal(t, rowsBefore[i], rowsAfter[i], "session row %d survives the rebuild", i)
	}

	indexesAfter := dumpRows(t, db, sessionIndexes)
	require.Len(t, indexesAfter, 2, "the operator contract adds the root history index")
	assert.Equal(t, indexesBefore[0], indexesAfter[0], "the existing sessions index is recreated verbatim")
	assert.Equal(t, "idx_sessions_root_history", indexesAfter[1]["name"].String)

	assert.Empty(t, dumpRows(t, db, `PRAGMA foreign_key_check`), "no dangling references after the rebuild")

	// A deleted high-id session must not have its id handed out again:
	// subagent_links has no foreign key to catch the collision.
	var seq int64
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT seq FROM sqlite_sequence WHERE name = 'sessions'`).Scan(&seq))
	assert.Equal(t, int64(100), seq, "the AUTOINCREMENT high-water mark survives")

	notNull, dflt, _ := columnConstraint(t, db, "sessions", "agent_type")
	assert.True(t, notNull)
	assert.False(t, dflt.Valid)
}

// seedSessionsAtV21 writes a representative session tree plus one row in every
// table that references sessions, as a real daemon at version 21 would have.
func seedSessionsAtV21(t *testing.T, db *sql.DB) {
	t.Helper()

	ctx := context.Background()

	exec := func(query string, args ...any) {
		t.Helper()
		_, err := db.ExecContext(ctx, query, args...)
		require.NoError(t, err, query)
	}

	exec(`INSERT INTO projects (id, work_dir, name) VALUES (1, '/tmp/p', 'p')`)

	exec(`INSERT INTO sessions
		(id, project_id, model, reasoning_level, master_enabled, attributes, agent_type,
		 parent_id, iteration, status, todo_items, compaction_brief,
		 created_at, updated_at, killed_at, root_id)
		VALUES (1, 1, 'sonnet', 'high', 1, '{"chat":"7"}', 'build',
		 0, 12, 'active', '[{"t":"x"}]', 'brief',
		 '2026-01-01 00:00:00', '2026-01-02 00:00:00', NULL, 0)`)
	exec(`INSERT INTO sessions (id, project_id, model, agent_type, parent_id, root_id, killed_at)
		VALUES (2, 1, 'haiku', 'general', 1, 1, '2026-01-03 00:00:00')`)
	exec(`INSERT INTO sessions (id, project_id, model, agent_type, parent_id, root_id)
		VALUES (3, 1, 'haiku', 'explore', 1, 1)`)
	exec(`INSERT INTO sessions (id, project_id, model, agent_type, parent_id, root_id)
		VALUES (4, 1, 'm', NULL, 0, 0)`)
	exec(`INSERT INTO sessions (id, project_id, model, agent_type, parent_id, root_id)
		VALUES (5, 1, 'm', NULL, 1, 1)`)

	// A session that existed and was removed: only sqlite_sequence remembers it.
	exec(`INSERT INTO sessions (id, project_id, model, agent_type) VALUES (100, 1, 'm', 'build')`)
	exec(`DELETE FROM sessions WHERE id = 100`)

	exec(`INSERT INTO messages (id, session_id, role, content, position) VALUES (1, 1, 'user', 'task', 1)`)
	exec(`INSERT INTO schedules (id, session_id, cron_expr, input_message) VALUES (1, 1, '0 9 * * *', 'wake')`)
	exec(`INSERT INTO session_inbox (id, session_id, source, raw_content, received_at)
		VALUES (1, 1, 'user', 'queued', '2026-01-01 00:00:00')`)
	exec(`INSERT INTO session_deliveries (session_id, delivery_id, kind, fingerprint, delivered_at)
		VALUES (1, 'd1', 'tool_notification', 'fp', '2026-01-01 00:00:00')`)
	exec(`INSERT INTO subagent_links (parent_id, child_id, task_call_id, created_at) VALUES (1, 2, 'call', 100)`)
	exec(`INSERT INTO memories (project_id, text) VALUES (1, 'note')`)
}

// columnConstraint reports the NOT NULL flag and declared default of a column.
func columnConstraint(t *testing.T, db *sql.DB, table, column string) (bool, sql.NullString, bool) {
	t.Helper()

	for _, row := range dumpRows(t, db, "PRAGMA table_info("+table+")") {
		if row["name"].String == column {
			return row["notnull"].String == "1", row["dflt_value"], true
		}
	}

	return false, sql.NullString{}, false
}

// dumpRows reads a query into column-keyed rows so schema changes cannot hide
// behind a hand-written column list.
func dumpRows(t *testing.T, db *sql.DB, query string) []map[string]sql.NullString {
	t.Helper()

	rows, err := db.QueryContext(context.Background(), query)
	require.NoError(t, err, query)
	defer rows.Close()

	columns, err := rows.Columns()
	require.NoError(t, err)

	var out []map[string]sql.NullString

	for rows.Next() {
		values := make([]sql.NullString, len(columns))
		targets := make([]any, len(columns))

		for i := range values {
			targets[i] = &values[i]
		}

		require.NoError(t, rows.Scan(targets...))

		row := make(map[string]sql.NullString, len(columns))
		for i, name := range columns {
			row[name] = values[i]
		}

		out = append(out, row)
	}

	require.NoError(t, rows.Err())

	return out
}
