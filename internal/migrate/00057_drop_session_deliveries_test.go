package migrate

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrate_57_DropsSessionDeliveriesPreservingOtherTables(t *testing.T) {
	t.Parallel()

	db, err := OpenDB(t.Context(), filepath.Join(t.TempDir(), "drop-deliveries.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	provider := newProvider(t, db)
	_, err = provider.UpTo(t.Context(), 56)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO projects(id, work_dir, name) VALUES (1, '/work', 'work');
		INSERT INTO sessions(id, project_id, agent_type, attributes) VALUES (1, 1, 'build', '{"manager_id":"owner"}');
		INSERT INTO messages(id, session_id, role, content) VALUES (1, 1, 'user', 'task');
		INSERT INTO session_inbox(session_id, source, raw_content, received_at, delivery_key)
		VALUES (1, 'process', 'done', CURRENT_TIMESTAMP, 'process:1');
		INSERT INTO schedules(session_id, cron_expr, input_message) VALUES (1, '* * * * *', 'tick');
		INSERT INTO session_deliveries(session_id, delivery_id, kind, fingerprint, delivered_at)
		VALUES (1, 'legacy', 'tool_notification', 'fingerprint', CURRENT_TIMESTAMP)`)
	require.NoError(t, err)
	require.Equal(t, 1, countRows(t, db, "session_deliveries", "1 = 1"))
	const schema = `SELECT type, name, tbl_name, sql FROM sqlite_master
		WHERE tbl_name <> 'session_deliveries' ORDER BY type, name`
	schemaBefore := dumpRows(t, db, schema)
	tables := stringColumn(t, db, `SELECT name FROM sqlite_master WHERE type = 'table'
		AND name NOT IN ('session_deliveries', 'goose_db_version', 'sqlite_sequence') ORDER BY name`)
	queries := []string{`SELECT * FROM sqlite_sequence WHERE name <> 'goose_db_version' ORDER BY name`}
	for _, table := range tables {
		queries = append(queries, `SELECT * FROM "`+table+`"`)
	}
	before := make([][]map[string]sql.NullString, len(queries))
	for i, query := range queries {
		before[i] = dumpRows(t, db, query)
	}
	_, err = provider.UpTo(t.Context(), 57)
	require.NoError(t, err)
	assert.False(t, tableExists(t, db, "session_deliveries"))
	assert.Empty(t, dumpRows(t, db, `SELECT name FROM sqlite_master WHERE tbl_name = 'session_deliveries'`))
	assert.Equal(t, schemaBefore, dumpRows(t, db, schema))
	for i, query := range queries {
		assert.Equal(t, before[i], dumpRows(t, db, query), query)
	}
	assert.Empty(t, dumpRows(t, db, `PRAGMA foreign_key_check`))
}
