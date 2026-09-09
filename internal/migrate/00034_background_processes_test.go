package migrate

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sessionstore"
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
			deadline_at, created_at, advertised_at, host_intent, state
		) VALUES (
			'bgp_1', 1, 1, 'call_1', '/tmp/out/1.output',
			'2026-09-07 00:10:00', '2026-09-07 00:00:00', '2026-09-07 00:00:10', '',
			'running'
		)`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `
		INSERT INTO background_processes (
			id, session_id, root_session_id, tool_call_id, output_path,
			deadline_at, created_at, host_intent, state, exit_code, finished_at
		) VALUES (
			'bgp_2', 2, 1, 'call_2', '/tmp/out/2.output',
			'2026-09-07 00:10:00', '2026-09-07 00:00:00', '',
			'completed', 0, '2026-09-07 00:05:00'
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
			deadline_at, created_at, state
		) VALUES (
			'bgp_1', 1, 1, 'call_1', '/tmp/out/1.output',
			'2026-09-07 00:10:00', '2026-09-07 00:00:00', 'running'
		)`)
	require.NoError(t, err)

	var count int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM background_processes`,
	).Scan(&count))
	assert.Equal(t, 1, count)
}

func TestMigrate_36_AsyncCompletionInboxTransfersOnlyOwedProcessFacts(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "async-completion-upgrade.db")
	db, err := OpenDB(ctx, dbPath)
	require.NoError(t, err)
	defer db.Close()

	provider := newProvider(t, db)
	_, err = provider.UpTo(ctx, 35)
	require.NoError(t, err)
	seedBackgroundProcessSessions(ctx, t, db)
	_, err = db.ExecContext(ctx, `UPDATE sessions SET status = 'stopped' WHERE id = 1`)
	require.NoError(t, err)

	for _, id := range []string{"claimed_without_delivery", "pending", "claimed_with_delivery"} {
		_, err = db.ExecContext(ctx, `INSERT INTO background_processes (
			id, session_id, root_session_id, tool_call_id, output_path, deadline_at,
			created_at, advertised_at, exit_code, state, finished_at, delivery_state,
			delivery_target_session_id
		) VALUES (?, 1, 1, 'call', '/tmp/out', '2026-09-09 00:10:00',
			'2026-09-09 00:00:00', '2026-09-09 00:00:01', 0, 'completed',
			'2026-09-09 00:00:02', ?, ?)`, id, map[string]string{
			"claimed_without_delivery": "claimed", "pending": "pending", "claimed_with_delivery": "claimed",
		}[id], func() any {
			if id == "pending" {
				return nil
			}
			return 1
		}())
		require.NoError(t, err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO background_processes (
		id, session_id, root_session_id, tool_call_id, output_path, deadline_at,
		created_at, advertised_at, exit_code, state, finished_at, delivery_state
	) VALUES
		('pending_half<&', 1, 1, 'call', '/tmp/<out>&', '2026-09-09 00:10:00',
		 '2026-09-09 00:00:00', '2026-09-09 00:00:00.100', 0, 'completed',
		 '2026-09-09 00:00:00.500', 'pending'),
		('pending_hour', 1, 1, 'call', '/tmp/out', '2026-09-09 01:10:00',
		 '2026-09-09 00:00:00', '2026-09-09 00:00:00.100', 0, 'completed',
		 '2026-09-09 01:00:00', 'pending')`)
	require.NoError(t, err)
	createdAt := time.Date(2026, time.September, 9, 0, 0, 0, 250_000_000, time.UTC)
	_, err = db.ExecContext(ctx, `INSERT INTO background_processes (
		id, session_id, root_session_id, tool_call_id, output_path, deadline_at,
		created_at, advertised_at, exit_code, state, finished_at, delivery_state
	) VALUES ('go_time', 1, 1, 'call', '/tmp/out', ?, ?, ?, 0, 'completed', ?, 'pending')`,
		createdAt.Add(time.Hour), createdAt, createdAt.Add(time.Second), createdAt.Add(2*time.Second))
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO background_processes (
		id, session_id, root_session_id, tool_call_id, output_path, deadline_at,
		created_at, advertised_at, exit_code, state, finished_at, delivery_state
	) VALUES ('opaque_time', 1, 1, 'call', '/tmp/out', 'opaque deadline',
		'opaque start', 'opaque advertised', 0, 'completed', 'opaque finish', 'pending')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO session_deliveries
		(session_id, delivery_id, kind, fingerprint, delivered_at)
		VALUES (1, 'claimed_with_delivery', 'tool_notification', 'fingerprint', '2026-09-09 00:00:02')`)
	require.NoError(t, err)

	_, err = provider.Up(ctx)
	require.NoError(t, err)

	rows, err := db.QueryContext(
		ctx,
		`SELECT attributes, raw_content FROM session_inbox WHERE source = 'process' ORDER BY id`,
	)
	require.NoError(t, err)
	defer rows.Close()
	var contents []string
	for rows.Next() {
		var attributes, content string
		require.NoError(t, rows.Scan(&attributes, &content))
		assert.Contains(t, attributes, "process_id")
		contents = append(contents, content)
	}
	require.NoError(t, rows.Err())
	require.Len(t, contents, 6)
	assert.Contains(t, contents[0], "\nprocess_id: ")
	assert.Contains(t, contents[0], "\nduration: 2s\n")
	assert.NotContains(t, contents[0], "claimed_with_delivery")
	joined := strings.Join(contents, "\n")
	assert.Contains(t, joined, "process_id: pending_half&lt;&amp;")
	assert.Contains(t, joined, "duration: 500ms")
	assert.Contains(t, joined, "duration: 1h0m0s")
	var goTimeContent string
	for _, content := range contents {
		if strings.Contains(content, "process_id: go_time") {
			goTimeContent = content
			break
		}
	}
	assert.Contains(t, goTimeContent, "\nduration: 2s\n")
	var opaqueTimeContent string
	for _, content := range contents {
		if strings.Contains(content, "process_id: opaque_time") {
			opaqueTimeContent = content
			break
		}
	}
	assert.Contains(t, opaqueTimeContent, "\nduration: 0s\n")
	assert.Contains(t, joined, "output_file: /tmp/&lt;out&gt;&amp;")

	var status string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT status FROM sessions WHERE id = 1`).Scan(&status))
	assert.Equal(t, "stopped", status)
}

func TestMigrate_36_AsyncCompletionInboxNormalizesOpaqueLegacyTimestamp(t *testing.T) {
	ctx := context.Background()
	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "opaque-process-time.db"))
	require.NoError(t, err)
	defer db.Close()

	provider := newProvider(t, db)
	_, err = provider.UpTo(ctx, 35)
	require.NoError(t, err)
	seedBackgroundProcessSessions(ctx, t, db)
	_, err = db.ExecContext(ctx, `INSERT INTO background_processes (
		id, session_id, root_session_id, tool_call_id, output_path, deadline_at,
		created_at, advertised_at, exit_code, state, finished_at, delivery_state
	) VALUES ('opaque_time', 1, 1, 'call', '/tmp/out', 'opaque deadline',
		'opaque start', 'opaque advertised', 0, 'completed', 'opaque finish', 'pending')`)
	require.NoError(t, err)

	_, err = provider.Up(ctx)
	require.NoError(t, err)

	input, err := sessionstore.NewStore(db).PeekPending(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.InputSourceProcess, input.Source)
	assert.Contains(t, input.RawContent, "process_id: opaque_time")
	assert.Contains(t, input.RawContent, "\nduration: 0s\n")
	assert.Equal(t, time.Unix(0, 0).UTC(), input.ReceivedAt)
}

func TestMigrate_36_AsyncCompletionInboxCancelsKilledTreeInput(t *testing.T) {
	ctx := context.Background()
	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "killed-tree-input.db"))
	require.NoError(t, err)
	defer db.Close()

	provider := newProvider(t, db)
	_, err = provider.UpTo(ctx, 35)
	require.NoError(t, err)
	seedBackgroundProcessSessions(ctx, t, db)
	_, err = db.ExecContext(ctx, `INSERT INTO session_inbox
		(session_id, source, raw_content, received_at) VALUES
		(1, 'user', 'root pending', '2026-09-09 00:00:00'),
		(2, 'agent', 'child pending', '2026-09-09 00:00:01')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE sessions SET status = 'killed', killed_at = ? WHERE id = 1`,
		time.Date(2026, time.September, 9, 0, 0, 2, 0, time.UTC))
	require.NoError(t, err)

	_, err = provider.Up(ctx)
	require.NoError(t, err)

	rows, err := db.QueryContext(ctx, `SELECT state, resolution_reason FROM session_inbox ORDER BY id`)
	require.NoError(t, err)
	defer rows.Close()

	count := 0
	for rows.Next() {
		var state, reason string
		require.NoError(t, rows.Scan(&state, &reason))
		assert.Equal(t, string(sessionstore.InputStateCancelled), state)
		assert.Equal(t, "killed", reason)
		count++
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, 2, count)
}

func TestMigrate_36_AsyncCompletionInboxPreservesExistingRows(t *testing.T) {
	ctx := context.Background()
	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "async-inbox-preserve.db"))
	require.NoError(t, err)
	defer db.Close()

	provider := newProvider(t, db)
	_, err = provider.UpTo(ctx, 35)
	require.NoError(t, err)
	seedBackgroundProcessSessions(ctx, t, db)
	message, err := db.ExecContext(ctx, `INSERT INTO messages (session_id, role, content, created_at)
		VALUES (1, 'user', 'accepted', '2026-09-09 00:00:00')`)
	require.NoError(t, err)
	messageID, err := message.LastInsertId()
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO session_inbox
		(id, session_id, source, raw_content, attributes, received_at, state,
		 resolved_at, accepted_message_id)
		VALUES
		(41, 1, 'agent', 'pending', '{"kind":"pending"}', '2026-09-09 00:00:01', 'pending', NULL, NULL),
		(42, 1, 'user', 'accepted', '{"kind":"accepted"}', '2026-09-09 00:00:02', 'accepted',
			 '2026-09-09 00:00:03', ?)`, messageID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO session_tool_activations
		(input_id, session_id, tool_id, command, state, created_at)
		VALUES (41, 1, 'skill', '/skill test', 'pending', '2026-09-09 00:00:02')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO session_inbox
		(id, session_id, source, raw_content, received_at)
		VALUES (99, 1, 'user', 'deleted high water', '2026-09-09 00:00:04')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `DELETE FROM session_inbox WHERE id = 99`)
	require.NoError(t, err)

	_, err = provider.Up(ctx)
	require.NoError(t, err)

	rows, err := db.QueryContext(ctx, `SELECT id, source, raw_content, attributes, state,
		COALESCE(accepted_message_id, 0) FROM session_inbox ORDER BY id`)
	require.NoError(t, err)
	defer rows.Close()
	type inboxRow struct {
		id, messageID                      int64
		source, content, attributes, state string
	}
	var got []inboxRow
	for rows.Next() {
		var row inboxRow
		require.NoError(t, rows.Scan(
			&row.id, &row.source, &row.content, &row.attributes, &row.state, &row.messageID,
		))
		got = append(got, row)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []inboxRow{
		{id: 41, source: "agent", content: "pending", attributes: `{"kind":"pending"}`, state: "pending"},
		{
			id: 42, messageID: messageID, source: "user", content: "accepted",
			attributes: `{"kind":"accepted"}`, state: "accepted",
		},
	}, got)

	insert, err := db.ExecContext(ctx, `INSERT INTO session_inbox
		(session_id, source, raw_content, received_at) VALUES (1, 'process', 'next', '2026-09-09 00:00:04')`)
	require.NoError(t, err)
	nextID, err := insert.LastInsertId()
	require.NoError(t, err)
	assert.Greater(t, nextID, int64(99))

	var inputID, sessionID int64
	var toolID, command, state string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT input_id, session_id, tool_id, command, state
		FROM session_tool_activations`).Scan(&inputID, &sessionID, &toolID, &command, &state))
	assert.Equal(t, int64(41), inputID)
	assert.Equal(t, int64(1), sessionID)
	assert.Equal(t, "skill", toolID)
	assert.Equal(t, "/skill test", command)
	assert.Equal(t, "pending", state)

	fkRows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	require.NoError(t, err)
	defer fkRows.Close()
	assert.False(t, fkRows.Next(), "migration must leave no dangling foreign keys")
	require.NoError(t, fkRows.Err())
}
