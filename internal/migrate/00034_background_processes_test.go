package migrate

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedBackgroundProcessSessions(ctx context.Context, t *testing.T, db *sql.DB) {
	t.Helper()

	_, err := db.ExecContext(ctx, `INSERT INTO projects (id, work_dir, name) VALUES (1, '/tmp/p', 'p')`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `
		INSERT INTO sessions (id, project_id, model, agent_type)
		VALUES (1, 1, 'm', 'build')`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `
		INSERT INTO sessions (id, project_id, model, agent_type, parent_id, root_id)
		VALUES (2, 1, 'm', 'general', 1, 1)`)
	require.NoError(t, err)
}

// Migration 34 adds the durable background-process ledger with terminal-state
// and delivery-state invariants.
func TestMigrate_34_BackgroundProcessesFreshDB(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "bg-process-fresh.db")
	db, err := OpenDB(ctx, dbPath)
	require.NoError(t, err)
	defer db.Close()

	provider := newProvider(t, db)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	seedBackgroundProcessSessions(ctx, t, db)

	_, err = db.ExecContext(ctx, `
		INSERT INTO background_processes (
			id, session_id, root_session_id, tool_call_id, output_path,
			deadline_at, created_at, advertised_at, host_intent, state,
			delivery_state, delivery_target_session_id, delivered_at
		) VALUES (
			'bgp_1', 1, 1, 'call_1', '/tmp/out/1.output',
			'2026-09-07 00:10:00', '2026-09-07 00:00:00', '2026-09-07 00:00:10', '',
			'running', 'pending', NULL, NULL
		)`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `
		INSERT INTO background_processes (
			id, session_id, root_session_id, tool_call_id, output_path,
			deadline_at, created_at, host_intent, state, exit_code, finished_at,
			delivery_state, delivery_target_session_id, delivered_at
		) VALUES (
			'bgp_2', 2, 1, 'call_2', '/tmp/out/2.output',
			'2026-09-07 00:10:00', '2026-09-07 00:00:00', '',
			'completed', 0, '2026-09-07 00:05:00',
			'claimed', 1, NULL
		)`)
	require.NoError(t, err)

	var state string
	var exitCode *int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT state, exit_code FROM background_processes WHERE id = 'bgp_2'`,
	).Scan(&state, &exitCode))
	assert.Equal(t, "completed", state)
	require.NotNil(t, exitCode)
	assert.Equal(t, 0, *exitCode)

	cases := []struct {
		name string
		sql  string
	}{
		{
			name: "owner root mismatch",
			sql: `INSERT INTO background_processes (
					id, session_id, root_session_id, tool_call_id, output_path,
					deadline_at, created_at
				) VALUES (
					'bad_owner', 2, 2, 'call', '/tmp/x', '2026-09-07 00:10:00',
					'2026-09-07 00:00:00'
				)`,
		},
		{
			name: "invalid state",
			sql: `INSERT INTO background_processes (
					id, session_id, root_session_id, tool_call_id, output_path,
					deadline_at, created_at, state
				) VALUES (
					'bad1', 1, 1, 'call', '/tmp/x', '2026-09-07 00:10:00',
					'2026-09-07 00:00:00', 'exploded'
				)`,
		},
		{
			name: "invalid host intent",
			sql: `INSERT INTO background_processes (
					id, session_id, root_session_id, tool_call_id, output_path,
					deadline_at, created_at, host_intent
				) VALUES (
					'bad2', 1, 1, 'call', '/tmp/x', '2026-09-07 00:10:00',
					'2026-09-07 00:00:00', 'meteor'
				)`,
		},
		{
			name: "running row with finished_at",
			sql: `INSERT INTO background_processes (
					id, session_id, root_session_id, tool_call_id, output_path,
					deadline_at, created_at, state, finished_at
				) VALUES (
					'bad3', 1, 1, 'call', '/tmp/x', '2026-09-07 00:10:00',
					'2026-09-07 00:00:00', 'running', '2026-09-07 00:05:00'
				)`,
		},
		{
			name: "delivered row without target",
			sql: `INSERT INTO background_processes (
					id, session_id, root_session_id, tool_call_id, output_path,
					deadline_at, created_at, state, exit_code, finished_at,
					delivery_state
				) VALUES (
					'bad4', 1, 1, 'call', '/tmp/x', '2026-09-07 00:10:00',
					'2026-09-07 00:00:00', 'completed', 0, '2026-09-07 00:05:00',
					'delivered'
				)`,
		},
		{
			name: "claimed row with delivered_at",
			sql: `INSERT INTO background_processes (
					id, session_id, root_session_id, tool_call_id, output_path,
					deadline_at, created_at, state, exit_code, finished_at,
					delivery_state, delivery_target_session_id, delivered_at
				) VALUES (
					'bad5', 1, 1, 'call', '/tmp/x', '2026-09-07 00:10:00',
					'2026-09-07 00:00:00', 'completed', 0, '2026-09-07 00:05:00',
					'claimed', 1, '2026-09-07 00:06:00'
				)`,
		},
		{
			name: "completed row without exit code",
			sql: `INSERT INTO background_processes (
					id, session_id, root_session_id, tool_call_id, output_path,
					deadline_at, created_at, state, finished_at
				) VALUES (
					'bad6', 1, 1, 'call', '/tmp/x', '2026-09-07 00:10:00',
					'2026-09-07 00:00:00', 'completed', '2026-09-07 00:05:00'
				)`,
		},
		{
			name: "terminal row without finished time",
			sql: `INSERT INTO background_processes (
					id, session_id, root_session_id, tool_call_id, output_path,
					deadline_at, created_at, state, exit_code
				) VALUES (
					'bad7', 1, 1, 'call', '/tmp/x', '2026-09-07 00:10:00',
					'2026-09-07 00:00:00', 'failed', 1
				)`,
		},
		{
			name: "deadline intent with completed outcome",
			sql: `INSERT INTO background_processes (
					id, session_id, root_session_id, tool_call_id, output_path,
					deadline_at, created_at, host_intent, state, exit_code, finished_at
				) VALUES (
					'bad8', 1, 1, 'call', '/tmp/x', '2026-09-07 00:10:00',
					'2026-09-07 00:00:00', 'deadline', 'completed', 0,
					'2026-09-07 00:05:00'
				)`,
		},
		{
			name: "timed out row with exit code",
			sql: `INSERT INTO background_processes (
					id, session_id, root_session_id, tool_call_id, output_path,
					deadline_at, created_at, host_intent, state, exit_code, finished_at
				) VALUES (
					'bad9', 1, 1, 'call', '/tmp/x', '2026-09-07 00:10:00',
					'2026-09-07 00:00:00', 'deadline', 'timed_out', -1,
					'2026-09-07 00:05:00'
				)`,
		},
		{
			name: "pending delivery with target",
			sql: `INSERT INTO background_processes (
					id, session_id, root_session_id, tool_call_id, output_path,
					deadline_at, created_at, state, delivery_target_session_id
				) VALUES (
					'bad10', 1, 1, 'call', '/tmp/x', '2026-09-07 00:10:00',
					'2026-09-07 00:00:00', 'running', 1
				)`,
		},
		{
			name: "running row with claimed delivery",
			sql: `INSERT INTO background_processes (
					id, session_id, root_session_id, tool_call_id, output_path,
					deadline_at, created_at, state, delivery_state,
					delivery_target_session_id
				) VALUES (
					'bad11', 1, 1, 'call', '/tmp/x', '2026-09-07 00:10:00',
					'2026-09-07 00:00:00', 'running', 'claimed', 1
				)`,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := db.ExecContext(ctx, testCase.sql)
			assert.Error(t, err, "row must be rejected")
		})
	}
}

// Rows written before the migration cannot exist; the upgrade path only needs
// the table and its invariants to come into being on a v33 database.
func TestMigrate_34_BackgroundProcessesUpgradeFromV33(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "bg-process-upgrade.db")
	db, err := OpenDB(ctx, dbPath)
	require.NoError(t, err)
	defer db.Close()

	provider := newProvider(t, db)

	if _, err := provider.UpTo(ctx, 33); err != nil {
		t.Fatalf("migrate to 33: %v", err)
	}

	seedBackgroundProcessSessions(ctx, t, db)

	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	_, err = db.ExecContext(ctx, `
		INSERT INTO background_processes (
			id, session_id, root_session_id, tool_call_id, output_path,
			deadline_at, created_at, state, delivery_state
		) VALUES (
			'bgp_1', 1, 1, 'call_1', '/tmp/out/1.output',
			'2026-09-07 00:10:00', '2026-09-07 00:00:00', 'running', 'pending'
		)`)
	require.NoError(t, err)

	var count int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM background_processes`,
	).Scan(&count))
	assert.Equal(t, 1, count)
}
