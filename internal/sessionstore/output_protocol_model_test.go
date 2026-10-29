package sessionstore

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/migrate"
)

// The oracle tracks immutable identities independently for two owners across restarts.
func runOutputLogModel(t *testing.T, commands, template []byte) {
	t.Helper()
	store, db, project := newTemplateTestStore(t, template)
	var roots [2]int64
	for owner := range roots {
		root, err := store.CreateSession(
			t.Context(),
			project,
			"model",
			"",
			map[string]any{"manager_id": fmt.Sprint(owner)},
		)
		require.NoError(t, err)
		roots[owner] = root.ID
	}
	type outputIdentity struct {
		owner int
		key   byte
	}
	type fact struct {
		content string
		id      int64
	}
	expected := make(map[outputIdentity]fact)
	var order []outputIdentity
	for step, raw := range commands {
		owner, key, op := int(raw/32%2), raw%4, raw/4%8
		if op == 7 {
			store = testStore(db)
		} else {
			identity := outputIdentity{owner, key}
			content := fmt.Sprintf("value-%d", op%2)
			draft := OutputDraft{
				SessionID: roots[owner],
				Type:      OutputMessage,
				Content:   content,
				SourceKey: fmt.Sprint(key),
			}
			draft.Fingerprint = OutputFingerprint(draft.Type, content, draft.SessionID, nil)
			result, err := store.EnqueueOutput(t.Context(), draft)
			previous, exists := expected[identity]
			switch {
			case exists && previous.content != content:
				require.ErrorIs(t, err, ErrOutputConflict)
			case exists:
				require.NoError(t, err)
				assert.True(t, result.Existing)
				assert.Equal(t, previous.id, result.OutputID)
			default:
				require.NoError(t, err)
				assert.False(t, result.Existing)
				expected[identity] = fact{content, result.OutputID}
				order = append(order, identity)
			}
		}
		func() {
			rows, err := db.QueryContext(
				t.Context(),
				`SELECT id, session_id, source_key, content FROM session_outbox ORDER BY id`,
			)
			require.NoError(t, err)
			defer rows.Close()
			i := 0
			for rows.Next() {
				var id, session int64
				var key, content string
				require.NoError(t, rows.Scan(&id, &session, &key, &content))
				require.Less(t, i, len(order), "step %d", step)
				identity := order[i]
				assert.Equal(t, expected[identity].id, id)
				assert.Equal(t, expected[identity].content, content)
				assert.Equal(t, roots[identity.owner], session)
				assert.Equal(t, fmt.Sprint(identity.key), key)
				i++
			}
			require.NoError(t, rows.Err())
			assert.Len(t, order, i, "step %d", step)
		}()
	}
}

// newTemplateTestStore is newTestStore over an optional pre-migrated template
// file, with fsync latency removed: fuzzing explores the protocol state
// machine, not disk durability.
func newTemplateTestStore(t *testing.T, migratedDB []byte) (*Store, *sql.DB, int64) {
	t.Helper()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")

	if migratedDB != nil {
		require.NoError(t, os.WriteFile(dbPath, migratedDB, 0o600))
	} else {
		db, err := migrate.OpenDB(ctx, dbPath)
		require.NoError(t, err)
		require.NoError(t, migrate.Run(ctx, db, dbPath))
		require.NoError(t, db.Close())
	}

	db, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	_, err = db.ExecContext(ctx, "PRAGMA synchronous=OFF")
	require.NoError(t, err)

	res, err := db.ExecContext(
		ctx,
		`INSERT INTO projects (work_dir, name) VALUES (?, ?)`,
		t.TempDir(), "test",
	)
	require.NoError(t, err)
	projectID, err := res.LastInsertId()
	require.NoError(t, err)

	return testStore(db), db, projectID
}

func TestHarnessModel_OutputLogAppendReplayConflictAndRestart(t *testing.T) {
	runOutputLogModel(t, []byte{0, 0, 4, 1, 32, 36, 33, 28, 0, 32, 2, 34}, nil)
}

func FuzzManagerOutputProtocol(f *testing.F) {
	path := filepath.Join(f.TempDir(), "output-template.db")
	db, err := migrate.OpenDB(f.Context(), path)
	require.NoError(f, err)
	require.NoError(f, migrate.Run(f.Context(), db, ""))
	require.NoError(f, db.Close())
	template, err := os.ReadFile(path)
	require.NoError(f, err)
	f.Add([]byte{0, 0, 4, 1, 32, 36, 33, 28, 0, 32})
	f.Fuzz(func(t *testing.T, commands []byte) {
		if len(commands) > 48 {
			commands = commands[:48]
		}
		runOutputLogModel(t, commands, template)
	})
}
