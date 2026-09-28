package migrate

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrate_45_FreshSchemaOmitsUnusedColumns(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "fresh.db")
	db, err := OpenDB(ctx, dbPath)
	require.NoError(t, err)
	defer db.Close()

	require.NoError(t, Run(ctx, db, dbPath))
	assert.False(t, columnExists(t, db, "sessions", "master_enabled"))
	assert.False(t, columnExists(t, db, "subagent_links", "delivered_msg_id"))
}

func TestMigrate_45_UpgradePreservesSessionTreeAndDelivery(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "v44.db")
	db, err := OpenDB(ctx, dbPath)
	require.NoError(t, err)
	defer db.Close()

	provider := newProvider(t, db)
	_, err = provider.UpTo(ctx, 44)
	require.NoError(t, err)
	require.True(t, columnExists(t, db, "sessions", "master_enabled"))
	require.True(t, columnExists(t, db, "subagent_links", "delivered_msg_id"))

	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, work_dir, name)
		VALUES (1, '/tmp/migration-45', 'migration-45')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO sessions
		(id, project_id, agent_type, master_enabled, attributes, status)
		VALUES (1, 1, 'build', 1, '{"manager_id":"test"}', 'active')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO sessions
		(id, project_id, agent_type, parent_id, root_id, status)
		VALUES (2, 1, 'general', 1, 1, 'completed')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO messages (id, session_id, role, content)
		VALUES (9, 1, 'tool', 'delivered')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO session_inbox
		(id, session_id, source, raw_content, received_at, state, resolved_at, accepted_message_id)
		VALUES (7, 1, 'subagent', 'done', '2026-09-28 00:00:00', 'accepted',
		        '2026-09-28 00:00:01', 9)`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO subagent_links
		(parent_id, child_id, task_call_id, blocking, depth, state, delivered_at,
		 delivered_msg_id, delivered_input_id, created_at, result, outcome, activation_seq)
		VALUES (1, 2, 'call-1', 0, 1, 'completed', 1727481600,
		        9, 7, 1727481500, 'done', 'completed', 1)`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `INSERT INTO sessions (id, project_id, agent_type)
		VALUES (100, 1, 'build')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `DELETE FROM sessions WHERE id = 100`)
	require.NoError(t, err)

	const sessionIndexes = `SELECT name, sql FROM sqlite_master
		WHERE type = 'index' AND tbl_name = 'sessions' ORDER BY name`
	const linkIndexes = `SELECT name, sql FROM sqlite_master
		WHERE type = 'index' AND tbl_name = 'subagent_links' ORDER BY name`
	require.True(t, indexExists(t, db, "sessions", "idx_sessions_project_id"))
	require.True(t, indexExists(t, db, "sessions", "idx_sessions_management_root"))
	require.True(t, indexExists(t, db, "subagent_links", "idx_subagent_links_child"))
	require.True(t, indexExists(t, db, "subagent_links", "idx_subagent_links_undelivered"))
	sessionsBefore := dumpRows(t, db, `SELECT * FROM sessions ORDER BY id`)
	linksBefore := dumpRows(t, db, `SELECT * FROM subagent_links ORDER BY child_id`)
	indexesBefore := append(dumpRows(t, db, sessionIndexes), dumpRows(t, db, linkIndexes)...)
	for _, row := range sessionsBefore {
		delete(row, "master_enabled")
	}
	for _, row := range linksBefore {
		delete(row, "delivered_msg_id")
	}

	_, err = provider.Up(ctx)
	require.NoError(t, err)

	assert.False(t, columnExists(t, db, "sessions", "master_enabled"))
	assert.False(t, columnExists(t, db, "subagent_links", "delivered_msg_id"))
	assert.Equal(t, sessionsBefore, dumpRows(t, db, `SELECT * FROM sessions ORDER BY id`))
	assert.Equal(t, linksBefore, dumpRows(t, db, `SELECT * FROM subagent_links ORDER BY child_id`))
	assert.Equal(t, indexesBefore,
		append(dumpRows(t, db, sessionIndexes), dumpRows(t, db, linkIndexes)...))
	assert.Empty(t, dumpRows(t, db, `PRAGMA foreign_key_check`))

	var seq int64
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT seq FROM sqlite_sequence WHERE name = 'sessions'`).Scan(&seq))
	assert.Equal(t, int64(100), seq)
	result, err := db.ExecContext(ctx, `INSERT INTO sessions (project_id, agent_type)
		VALUES (1, 'build')`)
	require.NoError(t, err)
	nextID, err := result.LastInsertId()
	require.NoError(t, err)
	assert.Equal(t, int64(101), nextID)

	var deliveredAt, deliveredInputID sql.NullInt64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT delivered_at, delivered_input_id
		FROM subagent_links WHERE child_id = 2`).Scan(&deliveredAt, &deliveredInputID))
	assert.Equal(t, sql.NullInt64{Int64: 1727481600, Valid: true}, deliveredAt)
	assert.Equal(t, sql.NullInt64{Int64: 7, Valid: true}, deliveredInputID)
}
