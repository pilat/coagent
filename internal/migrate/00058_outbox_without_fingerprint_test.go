package migrate

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrate_58_PreservesOutputRowsSequenceAndWatermarks(t *testing.T) {
	for _, empty := range []bool{false, true} {
		name := "populated"
		if empty {
			name = "empty after deletion"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			db, err := OpenDB(t.Context(), filepath.Join(t.TempDir(), "outbox.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			provider := newProvider(t, db)
			_, err = provider.UpTo(t.Context(), 57)
			require.NoError(t, err)
			seedOutboxBefore58(t, db)
			if empty {
				_, err = db.ExecContext(t.Context(), `DELETE FROM session_outbox`)
				require.NoError(t, err)
			}
			const rows = `SELECT id, session_id, type, content, attributes, source_key, created_at FROM session_outbox ORDER BY id`
			const indexes = `SELECT name, sql FROM sqlite_master WHERE type = 'index' AND tbl_name = 'session_outbox' ORDER BY name`
			const seen = `SELECT * FROM chat_seen`
			before, indexesBefore, seenBefore := dumpRows(t, db, rows), dumpRows(t, db, indexes), dumpRows(t, db, seen)
			require.Empty(t, dumpRows(t, db, `PRAGMA foreign_key_check`))
			_, err = provider.UpTo(t.Context(), 58)
			require.NoError(t, err)
			assert.Equal(t, before, dumpRows(t, db, rows))
			assert.Equal(t, indexesBefore, dumpRows(t, db, indexes))
			assert.Equal(t, seenBefore, dumpRows(t, db, seen))
			assert.False(t, columnExists(t, db, "session_outbox", "fingerprint"))
			assert.False(t, tableExists(t, db, "session_outbox_legacy_00058"))
			assert.Empty(t, dumpRows(t, db, `PRAGMA foreign_key_check`))
			result, err := db.ExecContext(
				t.Context(),
				`INSERT INTO session_outbox(session_id, type, content, attributes, created_at)
				VALUES (1, 'message', 'next', '{}', CURRENT_TIMESTAMP)`,
			)
			require.NoError(t, err)
			id, err := result.LastInsertId()
			require.NoError(t, err)
			assert.Greater(t, id, int64(90))
		})
	}
}

func seedOutboxBefore58(t *testing.T, db *sql.DB) {
	t.Helper()

	_, err := db.ExecContext(t.Context(), `
		INSERT INTO projects(id, work_dir, name) VALUES (1, '/work', 'work');
		INSERT INTO sessions(id, project_id, agent_type, attributes)
		VALUES (1, 1, 'build', '{"manager_id":"owner"}');
		INSERT INTO session_outbox
			(id, session_id, type, content, attributes, source_key, fingerprint, created_at)
		VALUES (10, 1, 'message', 'unkeyed', '{"manager_id":"owner"}', NULL, NULL, '2026-01-01'),
			(20, 1, 'message', 'keyed', '{"manager_id":"owner","source":"agent"}', 'answer', 'old', '2026-01-02'),
			(30, 1, 'session_opened', '', '{"manager_id":"owner","name":"work","work_dir":"/work"}', 'opened', 'old', '2026-01-03'),
			(90, 1, 'message', 'deleted', '{}', NULL, NULL, '2026-01-04');
		DELETE FROM session_outbox WHERE id = 90;
		INSERT INTO chat_seen VALUES ('owner', 'chat', 1, 20)`)
	require.NoError(t, err)
}
