package migrate

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrate_00051_DropsToolActivationsPreservingRows(t *testing.T) {
	t.Parallel()
	db, err := OpenDB(t.Context(), filepath.Join(t.TempDir(), "tool-activations.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	provider := newProvider(t, db)
	_, err = provider.UpTo(t.Context(), 50)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO projects(id, work_dir, name) VALUES (1, '/work', 'work');
		INSERT INTO sessions(id, project_id, agent_type, attributes, model, status)
		VALUES (1, 1, 'build', '{"manager_id":"owner"}', 'model', 'active');
		INSERT INTO session_inbox(id, session_id, source, raw_content, received_at)
		VALUES (1, 1, 'user', '/budget pending', CURRENT_TIMESTAMP),
			(2, 1, 'user', '/budget consumed', CURRENT_TIMESTAMP),
			(3, 1, 'user', '/config expired', CURRENT_TIMESTAMP);
		INSERT INTO session_tool_activations(input_id, session_id, tool_id, command, state, tool_call_id, created_at, resolved_at)
		VALUES (1, 1, 'set_budget', '/budget', 'pending', NULL, CURRENT_TIMESTAMP, NULL),
			(2, 1, 'set_budget', '/budget', 'consumed', 'budget-call', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP),
			(3, 1, 'config_edit', '/config', 'expired', NULL, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`)
	require.NoError(t, err)
	sessions := dumpRows(t, db, `SELECT * FROM sessions ORDER BY id`)
	inputs := dumpRows(t, db, `SELECT * FROM session_inbox ORDER BY id`)
	_, err = provider.UpTo(t.Context(), 51)
	require.NoError(t, err)
	assert.Equal(t, sessions, dumpRows(t, db, `SELECT * FROM sessions ORDER BY id`))
	assert.Equal(t, inputs, dumpRows(t, db, `SELECT * FROM session_inbox ORDER BY id`))
	assert.False(t, tableExists(t, db, "session_tool_activations"))
	assert.Empty(
		t,
		dumpRows(
			t,
			db,
			`SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'idx_session_tool_activations_pending'`,
		),
	)
	assert.Empty(t, dumpRows(t, db, `PRAGMA foreign_key_check`))
	_, err = provider.Up(t.Context())
	require.NoError(t, err)
}
