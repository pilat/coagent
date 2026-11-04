package migrate

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrate_00059_RemovesGraphAndSeedsCursor(t *testing.T) {
	t.Parallel()
	db, err := OpenDB(t.Context(), filepath.Join(t.TempDir(), "groupless.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	provider := newProvider(t, db)
	_, err = provider.UpTo(t.Context(), 58)
	require.NoError(t, err)
	seedGrouplessBefore59(t, db)
	require.Empty(t, dumpRows(t, db, `PRAGMA foreign_key_check`))
	var sequence int64
	require.NoError(t, db.QueryRowContext(t.Context(),
		`SELECT seq FROM sqlite_sequence WHERE name = 'session_outbox'`).Scan(&sequence))
	_, err = provider.Up(t.Context())
	require.NoError(t, err)
	assertGrouplessSchema(t, db)
	for _, table := range []string{
		"sessions", "messages", "schedules", "session_inbox", "session_outbox",
		"session_tool_activations", "session_budgets", "session_file_reads", "background_processes", "subagent_links",
	} {
		column := "session_id"
		switch table {
		case "sessions":
			column = "id"
		case "session_budgets":
			column = "root_session_id"
		case "subagent_links":
			column = "parent_id"
		}
		var count int
		require.NoError(t, db.QueryRowContext(t.Context(),
			fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s BETWEEN 10 AND 15`, table, column)).Scan(&count))
		assert.Zero(t, count, table)
	}
	var links int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM subagent_links`).Scan(&links))
	assert.Zero(t, links, "links from surviving parents to doomed children must also be deleted")
	for _, table := range []string{"projects", "memories", "mcp_servers"} {
		column := "project_id"
		if table == "projects" {
			column = "id"
		}
		var count int
		require.NoError(t, db.QueryRowContext(t.Context(),
			fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s = 2`, table, column)).Scan(&count))
		assert.Zero(t, count, table)
	}
	for _, table := range []string{"memories", "mcp_servers"} {
		var kept int
		require.NoError(t, db.QueryRowContext(t.Context(),
			fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE project_id = 1`, table)).Scan(&kept))
		assert.Equal(t, 1, kept, table)
	}
	for _, check := range []struct{ query, want string }{
		{`SELECT attributes FROM sessions WHERE id = 20`, `{"manager_id":"owner","title":"kept"}`},
		{`SELECT attributes FROM session_inbox WHERE id = 20`, `{"manager_id":"owner","fresh":true,"keep":"input"}`},
		{`SELECT attributes FROM session_outbox WHERE id = 20`, `{"manager_id":"owner","message_ids":[20]}`},
		{`SELECT metadata FROM schedules WHERE id = 22`, `{"keep":"recurring"}`},
	} {
		var got string
		require.NoError(t, db.QueryRowContext(t.Context(), check.query).Scan(&got))
		assert.JSONEq(t, check.want, got)
	}
	for _, check := range []struct{ query, want string }{
		{`SELECT attributes FROM sessions WHERE id = 21`, `{ "manager_id": "owner" }`},
		{`SELECT attributes FROM session_inbox WHERE id = 21`, `{ "fresh": false }`},
		{`SELECT attributes FROM session_outbox WHERE id = 21`, `{ "message_ids": [21] }`},
		{`SELECT metadata FROM schedules WHERE id = 20`, `{ "tool_call_id": "sleep-call" }`},
		{`SELECT metadata FROM schedules WHERE id = 21`, `{ "keep": "once" }`},
	} {
		var got string
		require.NoError(t, db.QueryRowContext(t.Context(), check.query).Scan(&got))
		assert.Equal(t, check.want, got, "rows without retired keys must not be rewritten")
	}
	var schedules int
	require.NoError(t, db.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM schedules WHERE session_id = 20 AND id IN (20,21,22)`).Scan(&schedules))
	assert.Equal(t, 3, schedules)
	for _, check := range []struct {
		chat            string
		session, cursor sql.NullInt64
	}{
		{chat: "assistant"},
		{chat: "work", session: sql.NullInt64{Int64: 20, Valid: true}, cursor: sql.NullInt64{Int64: 20, Valid: true}},
		{chat: "unseen", session: sql.NullInt64{Int64: 21, Valid: true}},
		{chat: "none"},
	} {
		var session, cursor sql.NullInt64
		require.NoError(t, db.QueryRowContext(t.Context(),
			`SELECT session_id,last_sent_id FROM chat_focus WHERE manager_id = 'owner' AND chat_id = ?`,
			check.chat).Scan(&session, &cursor))
		assert.Equal(t, check.session, session, check.chat)
		assert.Equal(t, check.cursor, cursor, check.chat)
	}
	var after int64
	require.NoError(t, db.QueryRowContext(t.Context(),
		`SELECT seq FROM sqlite_sequence WHERE name = 'session_outbox'`).Scan(&after))
	assert.Equal(t, sequence, after)
	assert.Empty(t, dumpRows(t, db, `PRAGMA foreign_key_check`))
}

func TestMigrate_00059_EmptyDatabase(t *testing.T) {
	t.Parallel()
	db, err := OpenDB(t.Context(), filepath.Join(t.TempDir(), "empty.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = newProvider(t, db).Up(t.Context())
	require.NoError(t, err)
	assertGrouplessSchema(t, db)
	assert.Empty(t, dumpRows(t, db, `PRAGMA foreign_key_check`))
}

func assertGrouplessSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, table := range []string{"chat_seen", "assistant_memory", "assistant_focus", "assistant_search", "assistant_search_state"} {
		assert.False(t, tableExists(t, db, table), table)
	}
	var indexes int
	require.NoError(t, db.QueryRowContext(
		t.Context(),
		`SELECT COUNT(*) FROM sqlite_master WHERE name = 'idx_sessions_assistant_root' OR name LIKE 'assistant_search_%'`,
	).Scan(&indexes))
	assert.Zero(t, indexes)
	assert.True(t, columnExists(t, db, "chat_focus", "last_sent_id"))
	assert.True(t, columnExists(t, db, "projects", "hidden"))
}

func seedGrouplessBefore59(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.ExecContext(t.Context(), `
INSERT INTO projects(id,work_dir,name,hidden) VALUES (1,'/work','work',0),(2,'/sys','sys_coagent',1);
INSERT INTO sessions(id,project_id,agent_type,parent_id,root_id,status,killed_at,attributes) VALUES
 (10,2,'assistant',0,0,'completed',NULL,'{"manager_id":"owner"}'),
 (11,2,'general',10,10,'completed',NULL,'{}'),
 (12,2,'build',0,0,'killed',CURRENT_TIMESTAMP,'{}'),
 (13,1,'assistant',0,0,'completed',NULL,'{"manager_id":"other"}'),
 (14,1,'general',13,13,'completed',NULL,'{}'),
 (15,1,'general',0,13,'completed',NULL,'{}'),
 (20,1,'build',0,0,'completed',NULL,'{"manager_id":"owner","title":"kept","current_run":7}'),
 (21,1,'build',0,0,'completed',NULL,'{ "manager_id": "owner" }');
INSERT INTO messages(id,session_id,role,content) VALUES
 (10,10,'assistant','candidate'),(11,11,'user','child'),(12,12,'user','retired'),
 (13,13,'assistant','outside'),(20,20,'user','kept'),(21,21,'user','untouched');
UPDATE sessions SET completion_check_candidate_id=10 WHERE id=10;
INSERT INTO session_inbox(id,session_id,source,raw_content,received_at,attributes) VALUES
 (10,10,'user','/config',CURRENT_TIMESTAMP,'{}'),
 (11,11,'agent','child event',CURRENT_TIMESTAMP,'{}'),
 (20,20,'user','work',CURRENT_TIMESTAMP,
  '{"observer_session_id":10,"from":"assistant","forwarded_by":10,"forwarded_input_id":10,"observed_session_id":11,"observed_kind":"answered","observed_message_id":11,"schedule_id":10,"accepted_run":7,"manager_id":"owner","fresh":true,"keep":"input"}'),
 (21,21,'user','untouched',CURRENT_TIMESTAMP,'{ "fresh": false }');
INSERT INTO session_tool_activations(input_id,session_id,tool_id,command,state,created_at)
 VALUES(10,10,'config_edit','/config','pending',CURRENT_TIMESTAMP);
INSERT INTO session_outbox(id,session_id,type,content,attributes,source_key,created_at) VALUES
 (10,10,'message','desk','{}','message:10:reply',CURRENT_TIMESTAMP),
 (11,10,'message','relay','{"relayed_session_id":20}','relay:20',CURRENT_TIMESTAMP),
 (12,11,'message','child','{}',NULL,CURRENT_TIMESTAMP),
 (13,12,'session_closed','killed','{}',NULL,CURRENT_TIMESTAMP),
 (20,20,'message','work','{"manager_id":"owner","relayed_session_id":11,"relayed_output_id":12,"forwarded_session_id":11,"message_ids":[20]}',NULL,CURRENT_TIMESTAMP),
 (21,21,'message','untouched','{ "message_ids": [21] }',NULL,CURRENT_TIMESTAMP),
 (90,13,'message','outside','{}',NULL,CURRENT_TIMESTAMP);
INSERT INTO schedules(id,session_id,cron_expr,one_shot_at,input_message,metadata) VALUES
 (10,10,'0 9 * * *',NULL,'retired','{}'),
 (20,20,NULL,'2099-01-01','sleep','{ "tool_call_id": "sleep-call" }'),
 (21,20,NULL,'2099-01-02','once','{ "keep": "once" }'),
 (22,20,'0 9 * * *',NULL,'recurring','{"report_to":10,"keep":"recurring"}');
INSERT INTO assistant_memory(session_id,text,created_at,updated_at)
 VALUES(10,'note',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);
INSERT INTO assistant_focus(session_id,text,sessions,created_at,updated_at)
 VALUES(10,'focus','["20"]',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);
INSERT INTO assistant_search(rowid,body,scope) VALUES(1,'note',10);
INSERT INTO session_budgets(root_session_id,state,generation,armed_at,baseline_cost_usd,duration_seconds)
 VALUES(10,'armed',1,CURRENT_TIMESTAMP,0,60);
INSERT INTO session_file_reads(session_id,path,mtime_unix_nano,size,hash,updated_at)
 VALUES(11,'/sys/file',1,1,'hash',CURRENT_TIMESTAMP);
INSERT INTO background_processes(id,session_id,root_session_id,tool_call_id,output_path,deadline_at,created_at)
 VALUES('retired',11,10,'call','/output',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);
INSERT INTO subagent_links(parent_id,child_id,task_call_id,created_at) VALUES(10,11,'child',1),(20,14,'cross',1);
INSERT INTO memories(project_id,text) VALUES(2,'retired'),(1,'kept');
INSERT INTO mcp_servers(project_id,name,command,created_at,updated_at)
 VALUES(2,'retired','retired',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP),(1,'kept','kept',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);
INSERT INTO chat_seen VALUES('owner','assistant',10,11),('owner','work',20,20);
INSERT INTO chat_focus VALUES('owner','assistant',10),('owner','work',20),('owner','unseen',21),('owner','none',NULL);
`)
	require.NoError(t, err)
}
