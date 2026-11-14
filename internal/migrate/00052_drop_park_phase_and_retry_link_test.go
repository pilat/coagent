package migrate

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrate_00052_DropsParkPhaseAndRetryLinkPreservingRows(t *testing.T) {
	t.Parallel()

	db, err := OpenDB(t.Context(), filepath.Join(t.TempDir(), "park-and-retry.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	provider := newProvider(t, db)
	_, err = provider.UpTo(t.Context(), 51)
	require.NoError(t, err)
	seedParkAndRetryRows(t, db)
	const budgets = `SELECT root_session_id, state, generation, armed_at, baseline_cost_usd, cost_limit_usd,
		duration_seconds, fired_at, released_at, fired_reason, released_reason, observed_cost_usd
		FROM session_budgets ORDER BY root_session_id`
	const messages = `SELECT id, session_id, role, content, finish_type, rejected_reason FROM messages ORDER BY id`
	budgetsBefore := dumpRows(t, db, budgets)
	messagesBefore := dumpRows(t, db, messages)
	_, err = provider.UpTo(t.Context(), 52)
	require.NoError(t, err)
	assert.Equal(t, budgetsBefore, dumpRows(t, db, budgets))
	assert.Equal(t, messagesBefore, dumpRows(t, db, messages))
	assert.False(t, columnExists(t, db, "session_budgets", "park_phase"))
	assert.False(t, columnExists(t, db, "session_budgets", "park_owner"))
	assert.False(t, columnExists(t, db, "messages", "retry_of_message_id"))
	_, err = db.ExecContext(t.Context(), `INSERT INTO session_budgets
		(root_session_id, state, generation, armed_at, baseline_cost_usd, cost_limit_usd,
		 fired_at, fired_reason, observed_cost_usd)
		VALUES (4, 'fired', 1, CURRENT_TIMESTAMP, 0, 1, NULL, 'cost', 1)`)
	require.ErrorContains(t, err, "CHECK constraint failed")
	assert.Empty(t, dumpRows(t, db, `PRAGMA foreign_key_check`))
	_, err = provider.Up(t.Context())
	require.NoError(t, err)
}

func seedParkAndRetryRows(t *testing.T, db *sql.DB) {
	t.Helper()

	_, err := db.ExecContext(t.Context(), `INSERT INTO projects(id, work_dir, name) VALUES (1, '/work', 'work');
		INSERT INTO sessions(id, project_id, agent_type, attributes, model, status)
		VALUES (1, 1, 'build', '{"manager_id":"owner"}', 'model', 'active'),
			(2, 1, 'build', '{"manager_id":"owner"}', 'model', 'stopped'),
			(3, 1, 'build', '{"manager_id":"owner"}', 'model', 'active'),
			(4, 1, 'build', '{"manager_id":"owner"}', 'model', 'active');
		INSERT INTO session_budgets
		(root_session_id, state, generation, armed_at, baseline_cost_usd, cost_limit_usd,
		 duration_seconds, fired_at, released_at, fired_reason, released_reason, observed_cost_usd,
		 park_phase, park_owner)
		VALUES (1, 'fired', 1, '2026-01-01 00:00:00', 2, 1, 60, '2026-01-01 00:01:00', NULL,
			'cost', '', 3, 'draining', 'budget:1:1'),
			(2, 'fired', 2, '2026-01-02 00:00:00', 4, 2, 120, '2026-01-02 00:02:00', NULL,
			'duration', '', 5, 'parked', ''),
			(3, 'released', 3, '2026-01-03 00:00:00', 6, 3, 180,
			'2026-01-03 00:03:00', '2026-01-03 00:04:00', 'cost', 'resumed', 9, 'parked', '');
		INSERT INTO messages(id, session_id, role, content, finish_type, rejected_reason)
		VALUES (1, 1, 'assistant', 'cut off', 'length', 'output_length');
		INSERT INTO messages(id, session_id, role, content, retry_of_message_id)
		VALUES (2, 1, 'user', 'recover the response', 1)`)
	require.NoError(t, err)
}
