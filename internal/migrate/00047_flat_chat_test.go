package migrate

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrate_00047_PreservesOutputLogAndSequence(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprintf("empty=%t", empty), func(t *testing.T) {
			t.Parallel()
			db := flatChatPrestate(t)
			_, err := db.ExecContext(t.Context(), `
INSERT INTO projects(id, work_dir, name) VALUES (1, '/project', 'project');
INSERT INTO sessions(id, project_id, agent_type, attributes)
VALUES (1, 1, 'build', '{"manager_id":"owner"}');
INSERT INTO session_outbox(id, session_id, type, content, attributes, source_key, fingerprint, created_at)
VALUES (10, 1, 'message_persistent', 'unkeyed', '{"keep":true}', NULL, NULL, '2026-01-01'),
 (20, 1, 'message_replaceable', 'keyed', '{"manager_id":"owner","source":"agent"}', 'answer', 'old', '2026-01-02'),
 (30, 1, 'session_opened', '', '{"name":"project"}', 'opened', 'old', '2026-01-03'),
 (40, 1, 'session_replaced', '', '{"old_session_id":2}', NULL, NULL, '2026-01-04'),
 (50, 1, 'session_closed', 'closed', '{"reason":"killed"}', NULL, NULL, '2026-01-05'),
 (80, 1, 'message_replaceable', 'progress', '{}', 'progress:pending', 'old', '2026-01-06'),
 (90, 1, 'message_replaceable', 'progress delivered', '{}', 'progress:delivered', 'old', '2026-01-07');
UPDATE session_outbox SET state='delivered', attempt_seq=1,
 last_attempt_at='2026-01-07', delivered_at='2026-01-07' WHERE id=90;
INSERT INTO session_outbox(id, session_id, type, content, attributes, created_at)
VALUES (100, 1, 'message_persistent', 'deleted', '{}', '2026-01-08');
DELETE FROM session_outbox WHERE id=100;`)
			require.NoError(t, err)
			if empty {
				_, err = db.ExecContext(t.Context(), `DELETE FROM session_outbox`)
				require.NoError(t, err)
			}
			const expected = `SELECT id, session_id,
 CASE WHEN type IN ('message_persistent','message_replaceable') THEN 'message' ELSE type END AS type,
 content, attributes, source_key, created_at FROM session_outbox
 WHERE source_key IS NULL OR source_key NOT LIKE 'progress:%' ORDER BY id`
			before := dumpRows(t, db, expected)
			_, err = newProvider(t, db).UpTo(t.Context(), 47)
			require.NoError(t, err)
			assert.Equal(t, before, dumpRows(t, db, `SELECT * FROM session_outbox ORDER BY id`))
			assert.Zero(t, countRows(t, db, "session_outbox", "source_key LIKE 'progress:%'"))
			assertFlatChatSchema(t, db)
			result, err := db.ExecContext(t.Context(), `INSERT INTO session_outbox
 (session_id, type, content, attributes, created_at) VALUES (1, 'message', 'next', '{}', CURRENT_TIMESTAMP)`)
			require.NoError(t, err)
			id, err := result.LastInsertId()
			require.NoError(t, err)
			assert.Greater(t, id, int64(100))
			assert.Empty(t, dumpRows(t, db, `PRAGMA foreign_key_check`))
		})
	}
}

func TestMigrate_00047_DeletesManagementGraphAndStripsAttributes(t *testing.T) {
	t.Parallel()
	db := flatChatPrestate(t)
	seedManagementGraph(t, db)
	require.Empty(t, dumpRows(t, db, `PRAGMA foreign_key_check`))
	_, err := newProvider(t, db).UpTo(t.Context(), 47)
	require.NoError(t, err)
	for _, table := range []string{
		"sessions", "messages", "schedules", "session_inbox", "session_outbox", "session_tool_activations",
		"session_budgets", "session_file_reads", "session_deliveries", "background_processes",
	} {
		column := "session_id"
		switch table {
		case "sessions":
			column = "id"
		case "session_budgets":
			column = "root_session_id"
		}
		assert.Equal(t, []string{"20", "21"}, stringColumn(t, db,
			fmt.Sprintf(`SELECT CAST(%s AS TEXT) FROM %s ORDER BY %s`, column, table, column)), table)
	}
	assert.Equal(t, []string{"20:21"}, stringColumn(t, db,
		`SELECT parent_id || ':' || child_id FROM subagent_links ORDER BY parent_id,child_id`))
	for _, table := range []string{"projects", "memories", "mcp_servers"} {
		column := "project_id"
		if table == "projects" {
			column = "id"
		}
		assert.Equal(t, []string{"1"}, stringColumn(t, db,
			fmt.Sprintf(`SELECT CAST(%s AS TEXT) FROM %s`, column, table)), table)
	}
	for _, row := range []struct {
		id   int
		want string
	}{
		{20, `{"manager_id":"owner","keep":"root"}`},
		{21, `{"keep":"child"}`},
	} {
		var attributes string
		require.NoError(t, db.QueryRowContext(t.Context(),
			`SELECT attributes FROM sessions WHERE id = ?`, row.id).Scan(&attributes))
		assert.JSONEq(t, row.want, attributes)
	}
	assert.Zero(t, countRows(t, db, "sessions", `status <> 'completed' OR killed_at IS NOT NULL`))
	assert.Empty(t, stringColumn(t, db, `SELECT name FROM sqlite_master WHERE name = 'idx_sessions_management_root'`))
	assertFlatChatSchema(t, db)
	assert.Empty(t, dumpRows(t, db, `PRAGMA foreign_key_check`))
}

func flatChatPrestate(t *testing.T) *sql.DB {
	t.Helper()
	db, err := OpenDB(t.Context(), filepath.Join(t.TempDir(), "flat-chat.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = newProvider(t, db).UpTo(t.Context(), 46)
	require.NoError(t, err)
	return db
}

func assertFlatChatSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	assert.True(t, tableExists(t, db, "chat_focus"))
	assert.Zero(t, countRows(t, db, "chat_focus", ""))
	assert.Equal(t, []string{"manager_id", "chat_id", "session_id", "last_sent_id"},
		stringColumn(t, db, `SELECT name FROM pragma_table_info('chat_focus') ORDER BY cid`))
	assert.False(t, tableExists(t, db, "manager_bindings"))
	assert.False(t, columnExists(t, db, "sessions", "manager_reply_pending"))
	assert.Equal(t, []string{"idx_session_outbox_session_id", "idx_session_outbox_source_key"},
		stringColumn(t, db, `SELECT name FROM sqlite_master
 WHERE type='index' AND tbl_name='session_outbox' ORDER BY name`))
	assert.Equal(t, []string{"session_id", "id"},
		stringColumn(t, db, `SELECT name FROM pragma_index_info('idx_session_outbox_session_id') ORDER BY seqno`))
}

func seedManagementGraph(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.ExecContext(t.Context(), `
INSERT INTO projects(id,work_dir,name,hidden) VALUES (1,'/project','project',0),(2,'/sys','sys_coagent',1);
INSERT INTO sessions(id,project_id,agent_type,parent_id,root_id,status,killed_at,attributes) VALUES
 (10,2,'build',0,0,'completed',NULL,'{"manager_id":"owner","management_surface":true}'),
 (11,1,'general',10,10,'completed',NULL,'{}'),
 (12,2,'build',0,0,'killed',CURRENT_TIMESTAMP,'{}'),
 (13,1,'build',0,0,'completed',NULL,'{"manager_id":"other","management_surface":true}'),
 (14,1,'general',13,13,'completed',NULL,'{}'),
 (15,1,'general',999,13,'completed',NULL,'{}'),
 (16,1,'general',14,13,'completed',NULL,'{}'),
 (20,1,'build',0,0,'completed',NULL,'{"manager_id":"owner","telegram_topic_id":20,"keep":"root"}'),
 (21,1,'general',20,20,'completed',NULL,'{"management_surface":true,"keep":"child"}');
INSERT INTO messages(id,session_id,role,content) SELECT id,id,'user','history' FROM sessions;
UPDATE sessions SET completion_check_candidate_id=id WHERE id IN (10,13);
INSERT INTO session_inbox(id,session_id,source,raw_content,received_at)
 SELECT id,id,'user','/config',CURRENT_TIMESTAMP FROM sessions;
INSERT INTO session_tool_activations(input_id,session_id,tool_id,command,state,created_at)
 SELECT id,id,'config_edit','/config','pending',CURRENT_TIMESTAMP FROM sessions;
INSERT INTO session_outbox(id,session_id,type,content,attributes,created_at)
 SELECT id,id,'message_persistent','history','{}',CURRENT_TIMESTAMP FROM sessions;
INSERT INTO schedules(session_id,cron_expr,input_message)
 SELECT id,'0 9 * * *','scheduled' FROM sessions;
INSERT INTO session_budgets(root_session_id,state,generation,armed_at,baseline_cost_usd,duration_seconds)
 SELECT id,'armed',1,CURRENT_TIMESTAMP,0,60 FROM sessions;
INSERT INTO session_file_reads(session_id,path,mtime_unix_nano,size,hash,updated_at)
 SELECT id,'/file',1,1,'hash',CURRENT_TIMESTAMP FROM sessions;
INSERT INTO session_deliveries(session_id,delivery_id,kind,fingerprint,delivered_at)
 SELECT id,'legacy','tool_notification','legacy',CURRENT_TIMESTAMP FROM sessions;
INSERT INTO background_processes(id,session_id,root_session_id,tool_call_id,output_path,deadline_at,created_at)
 SELECT 'process:'||id,id,CASE WHEN parent_id=0 THEN id ELSE root_id END,
 'call','/output',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP FROM sessions;
INSERT INTO subagent_links(parent_id,child_id,task_call_id,created_at)
 VALUES (10,11,'hidden-child',1),(13,14,'outside-child',1),(14,16,'grandchild',1),
 (20,14,'cross-child',1),(20,21,'kept',1);
INSERT INTO memories(project_id,text) VALUES(2,'retired'),(1,'kept');
INSERT INTO mcp_servers(project_id,name,command,created_at,updated_at)
 VALUES(2,'retired','retired',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP),
 (1,'kept','kept',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);
INSERT INTO manager_bindings(manager_id,driver,attributes,created_at,updated_at)
 VALUES('owner','telegram','{"chat_id":1}',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);
`)
	require.NoError(t, err)
}
