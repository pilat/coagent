package migrate

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrate_00048_AssistantNotes(t *testing.T) {
	for _, previous := range []bool{false, true} {
		name := "fresh"
		if previous {
			name = "from_47"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "notes.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			provider := newProvider(t, db)
			if previous {
				_, err = provider.UpTo(ctx, 47)
				require.NoError(t, err)
				_, err = db.ExecContext(ctx, `INSERT INTO projects(id,work_dir,name) VALUES(1,'/p','p');
					INSERT INTO sessions(id,project_id,model,agent_type) VALUES(1,1,'m','build');
					INSERT INTO assistant_desk(session_id,diary,stickers,focus,updated_at) VALUES(1,
					'{"user.lang":"Russian","rule.safe":"ask first","open.google-docs":"already open","integration.google-docs":"OAuth missing","project.devbox":"PR 74","integration.other":"installed","git.policy":"squash only","renovate.followup":"done"}',
					'[]','keep focus','2026-10-17 02:10:40.186539774 +0000 UTC')`)
				require.NoError(t, err)
			}
			_, err = provider.UpTo(ctx, 48)
			require.NoError(t, err)
			assert.True(t, columnExists(t, db, "assistant_notes", "written_at"))
			if !previous {
				return
			}
			rows, err := db.QueryContext(ctx, `SELECT key,text,written_at FROM assistant_notes ORDER BY key`)
			require.NoError(t, err)
			defer rows.Close()
			got := map[string]string{}
			for rows.Next() {
				var key, text string
				var at int64
				require.NoError(t, rows.Scan(&key, &text, &at))
				assert.Zero(t, at)
				got[key] = text
			}
			require.NoError(t, rows.Err())
			require.NoError(t, rows.Close())
			assert.Equal(
				t,
				map[string]string{
					"user.lang":        "Russian",
					"rule.safe":        "ask first",
					"open.google-docs": "already open",
					"rule.git":         "squash only",
				},
				got,
			)
			var focus, diary string
			require.NoError(
				t,
				db.QueryRowContext(ctx, `SELECT focus,diary FROM assistant_desk WHERE session_id=1`).
					Scan(&focus, &diary),
			)
			assert.Equal(t, "keep focus", focus)
			assert.Contains(t, diary, "PR 74")
		})
	}
}

func TestMigrate_00048_PinnedStickersAndGoogleDocs(t *testing.T) {
	ctx := t.Context()
	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "notes.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	provider := newProvider(t, db)
	_, err = provider.UpTo(ctx, 47)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO projects(id,work_dir,name) VALUES(1,'/p','p');
		INSERT INTO sessions(id,project_id,model,agent_type) VALUES(1,1,'m','build');
		INSERT INTO assistant_desk(session_id,diary,stickers,updated_at) VALUES(1,
		'{"integration.google-docs":"OAuth missing","decision.old":"drop"}',
		'[{"key":"a","text":"pending","on":"758"},{"key":"duplicate","text":"second","on":"758"},{"key":"bad","text":"drop","on":"758x"},{"key":"empty","text":"drop","on":""},{"key":"raw","text":"drop","on":"devbox"}]',
		'2026-10-17 02:10:40.186539774 +0000 UTC')`)
	require.NoError(t, err)
	_, err = provider.UpTo(ctx, 48)
	require.NoError(t, err)
	var count int
	var text string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM assistant_notes`).Scan(&count))
	assert.Equal(t, 2, count)
	require.NoError(
		t,
		db.QueryRowContext(ctx, `SELECT text FROM assistant_notes WHERE key='open.google-docs'`).Scan(&text),
	)
	assert.Equal(t, "OAuth missing", text)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT text FROM assistant_notes WHERE key='wait.758'`).Scan(&text))
	assert.Equal(t, "pending", text)
}
