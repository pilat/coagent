package migrate

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Migration 42 adds durable completion-check state to sessions. Legacy rows
// start with no pending candidate, no reply obligation, and a zero empty
// streak, so historical terminals keep their recovery behavior.
func TestMigrate_42_CompletionCheckDefaultsLegacyRows(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "completion-check.db")
	db, err := OpenDB(ctx, dbPath)
	require.NoError(t, err)
	defer db.Close()

	provider := newProvider(t, db)

	if _, err := provider.UpTo(ctx, 41); err != nil {
		t.Fatalf("migrate to 41: %v", err)
	}

	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, work_dir, name) VALUES (1, '/tmp/p', 'p')`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `
		INSERT INTO sessions (id, project_id, model, agent_type)
		VALUES (1, 1, 'm', 'build')`)
	require.NoError(t, err)

	_, err = provider.Up(ctx)
	require.NoError(t, err)

	assert.True(t, columnExists(t, db, "sessions", "completion_check_candidate_id"),
		"sessions.completion_check_candidate_id must exist")
	assert.True(t, columnExists(t, db, "sessions", "manager_reply_pending"),
		"sessions.manager_reply_pending must exist")
	assert.True(t, columnExists(t, db, "sessions", "empty_stop_streak"),
		"sessions.empty_stop_streak must exist")

	var candidate sql.NullInt64
	var replyPending bool
	var streak int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT completion_check_candidate_id,
		manager_reply_pending, empty_stop_streak FROM sessions WHERE id = 1`).
		Scan(&candidate, &replyPending, &streak))
	assert.False(t, candidate.Valid, "a legacy row carries no pending candidate")
	assert.False(t, replyPending, "a legacy row owes no manager reply")
	assert.Zero(t, streak, "a legacy row starts with a zero empty streak")
}

func TestMigrate_42_CompletionCheckRoundTripsOnFreshDB(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "completion-check-fresh.db")
	db, err := OpenDB(ctx, dbPath)
	require.NoError(t, err)
	defer db.Close()

	provider := newProvider(t, db)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, work_dir, name) VALUES (1, '/tmp/p', 'p')`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `
		INSERT INTO sessions (id, project_id, model, agent_type)
		VALUES (1, 1, 'm', 'build')`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `
		INSERT INTO messages (id, session_id, role, content, finish_type)
		VALUES (1, 1, 'assistant', 'candidate', 'stop')`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `UPDATE sessions
		SET completion_check_candidate_id = 1, manager_reply_pending = 1, empty_stop_streak = 2
		WHERE id = 1`)
	require.NoError(t, err)

	var candidate sql.NullInt64
	var replyPending bool
	var streak int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT completion_check_candidate_id,
		manager_reply_pending, empty_stop_streak FROM sessions WHERE id = 1`).
		Scan(&candidate, &replyPending, &streak))
	require.True(t, candidate.Valid)
	assert.Equal(t, int64(1), candidate.Int64)
	assert.True(t, replyPending)
	assert.Equal(t, 2, streak)

	_, err = db.ExecContext(ctx, `UPDATE sessions SET empty_stop_streak = -1 WHERE id = 1`)
	assert.Error(t, err, "the empty streak must stay non-negative")
}
