package migrate

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrate_52_RetiresManagementTreesAndPreservesAssistantIdentity(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "retire-management.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	provider := newProvider(t, db)
	_, err = provider.UpTo(ctx, 51)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO projects(id, work_dir, name, hidden)
		VALUES (1, '/projects/sys_coagent', 'sys_coagent', 1);
		INSERT INTO sessions(id, project_id, agent_type, parent_id, root_id, status, attributes, killed_at) VALUES
		(1, 1, 'build', 0, 0, 'active', '{"manager_id":"one","management_surface":"service-topic","telegram_topic_id":11,"keep":"root"}', NULL),
		(2, 1, 'general', 1, 1, 'suspended', '{"telegram_topic_id":12,"keep":"child"}', NULL),
		(3, 1, 'general', 2, 1, 'active', '{"manager_id":"foreign-child","management_surface":true}', NULL),
		(4, 1, 'build', 0, 0, 'killed', '{"manager_id":"one","management_surface":true}', '2021-01-02 03:04:05'),
		(5, 1, 'build', 0, 0, 'active', '{"manager_id":"two","management_surface":true}', NULL),
		(6, 1, 'build', 0, 0, 'active', '{"manager_id":"three","management_surface":true}', NULL),
		(7, 1, 'build', 0, 0, 'active', '{"manager_id":"one","telegram_topic_id":17,"keep":"ordinary"}', NULL),
		(8, 1, 'general', 5, 5, 'active', '{}', NULL),
		(10, 1, 'assistant', 0, 0, 'active', '{"manager_id":"one","telegram_topic_id":19}', NULL),
		(11, 1, 'assistant', 0, 0, 'active', '{"manager_id":"three"}', NULL);
		INSERT INTO schedules(id, session_id, cron_expr, one_shot_at, input_message, metadata, fresh) VALUES
		(101, 1, '* * * * *', NULL, 'root schedule', '{}', 0),
		(102, 2, NULL, '2099-01-01 00:00:00', 'child one-shot', '{"keep":"scheduled"}', 1),
		(103, 3, '* * * * *', NULL, 'cron with metadata', '{"tool_call_id":"not-a-sleep"}', 0),
		(104, 1, NULL, '2099-01-01 00:00:00', 'sleep', '{"tool_call_id":"sleep-call"}', 0),
		(105, 4, '* * * * *', NULL, 'predecessor schedule', '{}', 0),
		(106, 5, '* * * * *', NULL, 'no assistant', '{}', 0),
		(107, 8, NULL, '2099-01-01 00:00:00', 'no assistant child', '{}', 0),
		(108, 6, '* * * * *', NULL, 'other owner', '{}', 0),
		(109, 7, '* * * * *', NULL, 'ordinary', '{}', 0),
		(110, 10, '* * * * *', NULL, 'assistant unchanged', '{}', 0);
		INSERT INTO session_budgets(root_session_id, state, generation, armed_at, baseline_cost_usd, duration_seconds)
		SELECT id, 'armed', 1, CURRENT_TIMESTAMP, 0, 60 FROM sessions WHERE id IN (1, 2, 3, 4, 5, 6, 7, 8, 10);
		INSERT INTO session_outbox(session_id, type, content, attributes, source_key, fingerprint, created_at)
		VALUES (1, 'message_persistent', 'history stays', '{"manager_id":"one"}', 'old:output', 'legacy', CURRENT_TIMESTAMP);
		INSERT INTO session_inbox(session_id, source, raw_content, received_at)
		VALUES (1, 'user', 'old request stays', CURRENT_TIMESTAMP)`)
	require.NoError(t, err)
	assistantIndex := stringColumn(t, db, `SELECT sql FROM sqlite_master WHERE name = 'idx_sessions_assistant_root'`)
	_, err = provider.UpTo(ctx, 52)
	require.NoError(t, err)
	assert.Equal(t, []string{"1", "2", "3", "4", "5", "6", "8"}, stringColumn(t, db,
		`SELECT CAST(id AS TEXT) FROM sessions WHERE status = 'killed' AND killed_at IS NOT NULL ORDER BY id`))
	assert.Equal(t, []string{"2021-01-02 03:04:05"}, stringColumn(t, db,
		`SELECT CAST(killed_at AS TEXT) FROM sessions WHERE id = 4`))
	assert.Equal(t, []string{"101:10", "102:10", "103:10", "105:10", "108:11", "109:7", "110:10"},
		stringColumn(t, db, `SELECT id || ':' || session_id FROM schedules ORDER BY id`))
	assert.Equal(t, []string{"1:scheduled"}, stringColumn(t, db,
		`SELECT fresh || ':' || json_extract(metadata, '$.keep') FROM schedules WHERE id = 102`))
	assert.Equal(t, []string{"7", "10"}, stringColumn(t, db,
		`SELECT CAST(root_session_id AS TEXT) FROM session_budgets ORDER BY root_session_id`))
	assert.Zero(t, countRows(t, db, "sessions", `json_type(attributes, '$.management_surface') IS NOT NULL
		OR json_type(attributes, '$.telegram_topic_id') IS NOT NULL`))
	assert.Equal(t, []string{"root", "child", "ordinary"}, stringColumn(t, db,
		`SELECT json_extract(attributes, '$.keep') FROM sessions WHERE id IN (1, 2, 7) ORDER BY id`))
	assert.Equal(t, []string{"7", "10", "11"}, stringColumn(t, db,
		`SELECT CAST(id AS TEXT) FROM sessions WHERE killed_at IS NULL AND status = 'active' ORDER BY id`))
	assert.Equal(t, []string{"10:one", "11:three"}, stringColumn(
		t,
		db,
		`SELECT id || ':' || json_extract(attributes, '$.manager_id') FROM sessions WHERE agent_type = 'assistant' ORDER BY id`,
	))
	assert.Equal(t, assistantIndex,
		stringColumn(t, db, `SELECT sql FROM sqlite_master WHERE name = 'idx_sessions_assistant_root'`))
	assert.Empty(t, stringColumn(t, db, `SELECT name FROM sqlite_master WHERE name = 'idx_sessions_management_root'`))
	assert.Equal(t, []string{"old:output:history stays"}, stringColumn(t, db,
		`SELECT source_key || ':' || content FROM session_outbox ORDER BY id`))
	assert.Equal(t, []string{"old request stays:pending"}, stringColumn(t, db,
		`SELECT raw_content || ':' || state FROM session_inbox ORDER BY id`))
	assert.Empty(t, stringColumn(t, db, `SELECT name FROM sqlite_temp_master WHERE name = 'retired_management_00052'`))
	assert.Empty(t, dumpRows(t, db, `PRAGMA foreign_key_check`))
}
