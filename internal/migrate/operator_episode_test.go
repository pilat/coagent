package migrate

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrate_OperatorEpisodeBackfillSelectsRecoverableModelWork(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "v26-operator-episode.db")
	db, err := OpenDB(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	provider := newProvider(t, db)
	_, err = provider.UpTo(ctx, 26)
	require.NoError(t, err)
	seedOperatorEpisodeCases(t, db)

	_, err = provider.UpTo(ctx, 27)
	require.NoError(t, err)

	for _, tc := range []struct {
		name    string
		id      int64
		started bool
		want    time.Time
	}{
		{name: "active model input", id: 1, started: true, want: mustTime(t, "2026-01-02T00:00:00Z")},
		{name: "read only input", id: 2},
		{name: "completed pending recovery", id: 3, started: true, want: mustTime(t, "2026-01-04T00:00:00Z")},
		{name: "active child", id: 4, started: true, want: mustTime(t, "2026-01-02T00:00:00Z")},
		{name: "idle completed history", id: 5},
		{name: "empty active root", id: 6},
		{name: "active scheduled work", id: 8, started: true, want: mustTime(t, "2026-01-02T00:00:00Z")},
		{name: "tab padded read only input", id: 9},
		{name: "uppercase compact is model input", id: 10, started: true, want: mustTime(t, "2026-01-02T00:00:00Z")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var episode sql.NullTime
			require.NoError(t, db.QueryRowContext(ctx,
				`SELECT episode_started_at FROM sessions WHERE id = ?`, tc.id).Scan(&episode))
			assert.Equal(t, tc.started, episode.Valid)
			if tc.started {
				assert.True(t, tc.want.Equal(episode.Time), "got %s", episode.Time)
			}
		})
	}
}

func seedOperatorEpisodeCases(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := t.Context()

	_, err := db.ExecContext(ctx, `INSERT INTO projects (id, work_dir, name) VALUES (1, '/tmp/p', 'p')`)
	require.NoError(t, err)
	for _, row := range []struct {
		id       int64
		status   string
		parentID int64
		rootID   int64
	}{
		{id: 1, status: "active"},
		{id: 2, status: "active"},
		{id: 3, status: "completed"},
		{id: 4, status: "completed"},
		{id: 5, status: "completed"},
		{id: 6, status: "active"},
		{id: 7, status: "active", parentID: 4, rootID: 4},
		{id: 8, status: "active"},
		{id: 9, status: "active"},
		{id: 10, status: "active"},
	} {
		agentType := "build"
		if row.parentID != 0 {
			agentType = "general"
		}
		_, err = db.ExecContext(ctx, `INSERT INTO sessions
			(id, project_id, model, agent_type, status, parent_id, root_id, created_at, updated_at)
			VALUES (?, 1, 'm', ?, ?, ?, ?, '2026-01-01 00:00:00', '2026-01-02 00:00:00')`,
			row.id, agentType, row.status, row.parentID, row.rootID)
		require.NoError(t, err)
	}

	_, err = db.ExecContext(ctx, `INSERT INTO messages (id, session_id, role, content)
		VALUES (1, 4, 'user', 'parent task'), (2, 5, 'user', 'old task')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO session_inbox
		(id, session_id, source, raw_content, received_at, state, resolved_at, accepted_message_id)
		VALUES
			(1, 1, 'user', 'active work', '2026-01-03 00:00:00', 'pending', NULL, NULL),
			(2, 2, 'user', '/status', '2026-01-03 00:00:00', 'pending', NULL, NULL),
			(3, 3, 'user', 'recover me', '2026-01-04 00:00:00', 'pending', NULL, NULL),
			(4, 4, 'user', 'spawn child', '2026-01-01 00:00:00', 'accepted', '2026-01-01 00:00:00', 1),
			(5, 5, 'user', 'old completed work', '2026-01-01 00:00:00', 'accepted', '2026-01-01 00:00:00', 2),
			(6, 9, 'user', char(9) || '/status' || char(10), '2026-01-03 00:00:00', 'pending', NULL, NULL),
			(7, 10, 'user', '/COMPACT focus', '2026-01-03 00:00:00', 'pending', NULL, NULL)`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO session_outbox
		(session_id, type, content, attributes, created_at)
		VALUES (8, 'message_persistent', 'scheduled', '{"manager_id":"telegram","source":"scheduler"}',
			'2026-01-03 00:00:00')`)
	require.NoError(t, err)
}

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()

	parsed, err := time.Parse(time.RFC3339, value)
	require.NoError(t, err)

	return parsed
}
