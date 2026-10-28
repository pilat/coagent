package migrate

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrate_00049_AssistantToolDesk(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "tool-desk.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	provider := newProvider(t, db)
	_, err = provider.UpTo(ctx, 48)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO projects(id,work_dir,name) VALUES(1,'/p','p');
		INSERT INTO sessions(id,project_id,model,agent_type) VALUES(1,1,'m','assistant'),(244,1,'m','build');
		INSERT INTO assistant_desk(session_id,focus,updated_at) VALUES(1,'keep focus','2026-10-17 02:10:40');
		INSERT INTO assistant_notes(session_id,key,text,written_at) VALUES
			(1,'user.lang','Russian',0),
			(1,'rule.git','squash only',1760000000),
			(1,'project.devbox','PR 74',0),
			(1,'session.244','Move the stand to podman',0),
			(1,'session.999','gone session',0),
			(1,'open.docs','Await OAuth',0),
			(1,'wait.244','Explain the redis failure',0),
			(1,'wait.devbox','left by a failed rekey',0);
		INSERT INTO assistant_tape(session_id,at,kind,text,hidden) VALUES
			(1,'2026-10-17 02:10:40','user','user: «старое кеширование»',1),
			(1,'2026-10-17 02:10:41','say','say: «ответ»',0),
			(1,'2026-10-17 02:10:42','move','→ devbox (244): "кеширование"',0);`)
	require.NoError(t, err)
	_, err = provider.UpTo(ctx, 49)
	require.NoError(t, err)

	assert.Equal(t, []string{
		"devbox: PR 74", "squash only", "session 999: gone session", "Russian",
	}, stringColumn(t, db, `SELECT text || COALESCE(until, '') FROM assistant_memory ORDER BY id`))
	assert.Equal(t, []string{
		`Await OAuth []`, `Explain the redis failure ["244"]`, `devbox: left by a failed rekey []`, `keep focus []`,
	}, stringColumn(t, db, `SELECT text || ' ' || sessions FROM assistant_focus ORDER BY id`))
	assert.Equal(t, []string{"244 Move the stand to podman"},
		stringColumn(t, db, `SELECT session_id || ' ' || about FROM assistant_work`))
	assert.Equal(t, []string{"2025-10-09 08:53:20"},
		stringColumn(t, db, `SELECT CAST(created_at AS TEXT) FROM assistant_memory WHERE text = 'squash only'`))
	assert.Equal(t, []string{"0"}, stringColumn(t, db, `SELECT message_id FROM assistant_search_state`))
	assert.Equal(t, []string{"1"}, stringColumn(t, db,
		`SELECT rowid % 4 FROM assistant_search WHERE assistant_search MATCH 'кеширование'`),
		"a hidden dialog line is searchable; a move line is not")
	assert.Len(t, stringColumn(t, db, `SELECT rowid FROM assistant_search WHERE rowid % 4 IN (2, 3)`), 8)

	for _, table := range []string{"assistant_notes", "assistant_desk"} {
		assert.Empty(t, stringColumn(t, db, `SELECT name FROM sqlite_master WHERE name = '`+table+`'`), table)
	}

	assert.True(t, columnExists(t, db, "assistant_turns", "phase"))
	_, err = db.ExecContext(
		ctx,
		`INSERT INTO assistant_work(session_id,about,created_at) VALUES(999,'x',CURRENT_TIMESTAMP)`,
	)
	require.ErrorContains(t, err, "FOREIGN KEY")
}

func TestMigrate_00049_Fresh(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "fresh.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = newProvider(t, db).Up(ctx)
	require.NoError(t, err)

	for _, table := range []string{"assistant_memory", "assistant_focus", "assistant_work"} {
		assert.True(t, columnExists(t, db, table, "session_id"), table)
	}
}

func stringColumn(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), query)
	require.NoError(t, err)
	defer rows.Close()
	var values []string
	for rows.Next() {
		var value string
		require.NoError(t, rows.Scan(&value))
		values = append(values, value)
	}
	require.NoError(t, rows.Err())

	return values
}
