package migrate

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sessionstore"
)

// Regression: the backfill used ON CONFLICT(session_id, source_key), which
// SQLite rejects because the only uniqueness is a partial index. A real
// database with a manager-owned root hit this on boot; a fresh test database
// never did because it had no rows to backfill.
func TestMigrate_ManagerOutboxBackfillSeedsExistingManagerRoots(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "v24.db")
	db, err := OpenDB(ctx, dbPath)
	require.NoError(t, err)
	defer db.Close()

	provider := newProvider(t, db)
	_, err = provider.UpTo(ctx, 24)
	require.NoError(t, err)

	seedManagerOwnedSessionsAtV24(t, db)

	_, err = provider.Up(ctx)
	require.NoError(t, err)

	rows := dumpRows(
		t,
		db,
		`SELECT session_id, type, attributes, source_key, fingerprint FROM session_outbox ORDER BY id`,
	)
	require.Len(t, rows, 1, "exactly one addressable manager root gets a lifecycle row")

	assert.Equal(t, "152", rows[0]["session_id"].String)
	assert.Equal(t, "session_opened", rows[0]["type"].String)
	assert.JSONEq(t,
		`{"manager_id":"telegram-main","name":"p","work_dir":"/tmp/p"}`,
		rows[0]["attributes"].String)
	assert.Equal(t, "session:152:opened", rows[0]["source_key"].String)

	// The stored fingerprint must match the store's own contract for the same
	// row, so a future re-enqueue replays as an idempotent no-op.
	require.Equal(t,
		sessionstore.OutputFingerprint(
			sessionstore.OutputSessionOpened, "", 152,
			map[string]any{"name": "p", "work_dir": "/tmp/p"},
		),
		rows[0]["fingerprint"].String,
		"backfill fingerprint must equal sessionstore.OutputFingerprint",
	)
}

// seedManagerOwnedSessionsAtV24 writes the boundary cases: one live
// manager-owned root (backfilled), plus roots that must be skipped.
func seedManagerOwnedSessionsAtV24(t *testing.T, db *sql.DB) {
	t.Helper()

	ctx := context.Background()

	exec := func(query string, args ...any) {
		t.Helper()
		_, err := db.ExecContext(ctx, query, args...)
		require.NoError(t, err, query)
	}

	exec(`INSERT INTO projects (id, work_dir, name) VALUES (1, '/tmp/p', 'p')`)

	owned := func(id int64, status string, killedAt any, parentID int64, attrs string) {
		t.Helper()
		exec(`INSERT INTO sessions (id, project_id, model, agent_type, parent_id, root_id,
			status, killed_at, attributes, created_at, updated_at)
			VALUES (?, 1, 'm', 'build', ?, ?, ?, ?, ?, '2026-01-01 00:00:00', '2026-01-02 00:00:00')`,
			id, parentID, id, status, killedAt, attrs)
	}

	// The live manager-owned root that triggered the production failure.
	owned(152, "active", nil, 0, `{"manager_id":"telegram-main"}`)
	// Ownerless legacy CLI root: skipped.
	owned(153, "active", nil, 0, `{}`)
	// Killed manager root: its surface is gone.
	owned(154, "killed", "2026-01-03 00:00:00", 0, `{"manager_id":"telegram-main"}`)
	// Terminating root: clear/kill cleanup owns it, not reconciliation.
	owned(155, "terminating", nil, 0, `{"manager_id":"telegram-main"}`)
	// Manager-owned subagent: only roots own manager output.
	owned(156, "active", nil, 152, `{"manager_id":"telegram-main"}`)

	var count int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions`).Scan(&count))
	require.Equal(t, 5, count)
}
