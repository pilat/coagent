package migrate

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/migrations"
)

type assistantSchemaRow struct{ kind, name, table, sql string }

func TestMigrate_00047_00050_Convergence(t *testing.T) {
	fresh, freshProvider := assistantSquashDB(t, false)
	_, err := freshProvider.Up(t.Context())
	require.NoError(t, err)
	legacy, legacyProvider := assistantSquashDB(t, true)
	applied, err := legacyProvider.Up(t.Context())
	require.NoError(t, err)
	require.Len(t, applied, 1)
	assert.EqualValues(t, 50, applied[0].Source.Version)
	assert.Equal(t, assistantSchema(t, fresh), assistantSchema(t, legacy))
}

func TestMigrate_00047_00050_Backfill(t *testing.T) {
	db, provider := assistantSquashDB(t, true)
	_, err := db.ExecContext(t.Context(), `INSERT INTO projects(id,work_dir,name) VALUES(1,'/p','p');
INSERT INTO sessions(id,project_id,model,agent_type,attributes) VALUES
(1,1,'model','assistant','{"manager_id":"owner"}'),(2,1,'model','build','{"manager_id":"owner"}');
INSERT INTO messages(id,session_id,role,content,created_at) VALUES
(1,2,'user','old request',CURRENT_TIMESTAMP),(2,2,'assistant','kept answer',CURRENT_TIMESTAMP);
INSERT INTO session_inbox(id,session_id,source,raw_content,attributes,received_at,state,resolved_at,accepted_message_id) VALUES
(1,2,'user','unanswered','{"from":"assistant","keep":"unanswered"}',CURRENT_TIMESTAMP,'pending',NULL,NULL),
(2,2,'user','answered','{"from":"assistant","keep":"answered"}',CURRENT_TIMESTAMP,'accepted',CURRENT_TIMESTAMP,1);
INSERT INTO assistant_sends(input_id,assistant_session_id,session_id,created_at,observed_at) VALUES
(1,1,2,CURRENT_TIMESTAMP,NULL),(2,1,2,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);
INSERT INTO assistant_memory(id,session_id,text,created_at,updated_at) VALUES(1,1,'kept memory',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);
INSERT INTO assistant_focus(id,session_id,text,sessions,created_at,updated_at) VALUES(1,1,'kept focus','["2"]',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);
INSERT INTO assistant_work(session_id,about,created_at) VALUES(2,'kept work',CURRENT_TIMESTAMP);
INSERT INTO assistant_tape(id,session_id,at,kind,text) VALUES(1,1,CURRENT_TIMESTAMP,'say','discarded tape');
INSERT INTO assistant_turns(session_id,input_id,call,phase,request,response,outcome,created_at)
VALUES(1,1,1,'main','desk','answer','ok',CURRENT_TIMESTAMP);
INSERT INTO assistant_search(rowid,body,scope) VALUES(5,'discarded tape',1),(6,'kept memory',1),(7,'kept focus',1),(8,'kept answer',2);
UPDATE assistant_search_state SET message_id=2 WHERE id=1;`)
	require.NoError(t, err)
	_, err = provider.Up(t.Context())
	require.NoError(t, err)
	attrs := stringColumn(t, db, `SELECT attributes FROM session_inbox ORDER BY id`)
	require.Len(t, attrs, 2)
	assert.JSONEq(t, `{"from":"assistant","keep":"unanswered","observer_session_id":1}`, attrs[0])
	assert.JSONEq(t, `{"from":"assistant","keep":"answered"}`, attrs[1])
	assert.Equal(t, []string{"kept memory"}, stringColumn(t, db, `SELECT text FROM assistant_memory`))
	assert.Equal(
		t,
		[]string{`kept focus ["2"]`},
		stringColumn(t, db, `SELECT text || ' ' || sessions FROM assistant_focus`),
	)
	assert.Equal(t, []string{"kept work"}, stringColumn(t, db, `SELECT about FROM assistant_work`))
	assert.Equal(
		t,
		[]string{"kept memory", "kept focus", "kept answer"},
		stringColumn(t, db, `SELECT body FROM assistant_search ORDER BY rowid`),
	)
	assert.Equal(t, []string{"2"}, stringColumn(t, db, `SELECT message_id FROM assistant_search_state`))
	assert.False(t, tableExists(t, db, "assistant_sends"))
}

func TestMigrate_00047_00050_FreshIdempotence(t *testing.T) {
	db, provider := assistantSquashDB(t, false)
	_, err := provider.Up(t.Context())
	require.NoError(t, err)
	before := assistantSchema(t, db)
	cleanup, err := migrations.FS.ReadFile("00050_assistant_session.sql")
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), string(cleanup))
	require.NoError(t, err)
	assert.Equal(t, before, assistantSchema(t, db))
	for _, table := range []string{"assistant_sends", "assistant_tape", "assistant_turns", "assistant_notes", "assistant_desk"} {
		assert.False(t, tableExists(t, db, table), table)
	}
	for _, table := range []string{"assistant_memory", "assistant_focus", "assistant_work", "assistant_search", "assistant_search_state"} {
		assert.True(t, tableExists(t, db, table), table)
	}
	assert.Equal(t, []string{"0"}, stringColumn(t, db, `SELECT message_id FROM assistant_search_state`))
	for _, file := range []string{"00048_assistant_notes.sql", "00049_assistant_tool_desk.sql"} {
		_, err := migrations.FS.ReadFile(file)
		require.ErrorIs(t, err, os.ErrNotExist)
	}
}

func TestMigrate_00047_00050_UniqueRootIndex(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "fresh"
		if legacy {
			name = "legacy"
		}
		t.Run(name, func(t *testing.T) {
			db, provider := assistantSquashDB(t, legacy)
			_, err := provider.Up(t.Context())
			require.NoError(t, err)
			_, err = db.ExecContext(t.Context(), `INSERT INTO projects(id,work_dir,name) VALUES(1,'/p','p');
INSERT INTO sessions(project_id,model,agent_type,attributes) VALUES(1,'m','assistant','{"manager_id":"owner"}');`)
			require.NoError(t, err)
			_, err = db.ExecContext(t.Context(), `INSERT INTO sessions(project_id,model,agent_type,attributes)
VALUES(1,'m','assistant','{"manager_id":"owner"}')`)
			require.ErrorContains(t, err, "UNIQUE")
		})
	}
}

func assistantSquashDB(t *testing.T, legacy bool) (*sql.DB, *goose.Provider) {
	t.Helper()
	db, err := OpenDB(t.Context(), filepath.Join(t.TempDir(), "assistant-squash.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	provider := newProvider(t, db)
	if legacy {
		_, err := provider.UpTo(t.Context(), 46)
		require.NoError(t, err)
		body, err := os.ReadFile(filepath.Join("testdata", "assistant_00047_00049.sql"))
		require.NoError(t, err)
		_, err = db.ExecContext(t.Context(), string(body))
		require.NoError(t, err)
		_, err = db.ExecContext(
			t.Context(),
			`INSERT INTO goose_db_version(version_id,is_applied) VALUES(47,1),(48,1),(49,1)`,
		)
		require.NoError(t, err)
	}
	return db, provider
}

func assistantSchema(t *testing.T, db *sql.DB) []assistantSchemaRow {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT type, name, tbl_name, sql FROM sqlite_master
WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name`)
	require.NoError(t, err)
	defer rows.Close()
	var schema []assistantSchemaRow
	for rows.Next() {
		var row assistantSchemaRow
		var raw sql.NullString
		require.NoError(t, rows.Scan(&row.kind, &row.name, &row.table, &raw))
		row.sql = strings.Join(strings.Fields(raw.String), " ")
		schema = append(schema, row)
	}
	require.NoError(t, rows.Err())
	return schema
}
