package migrate

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrate_37_AddsAgentProcessCancellationIntent(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "agent-process-cancellation.db")
	db, err := OpenDB(ctx, dbPath)
	require.NoError(t, err)
	defer db.Close()

	provider := newProvider(t, db)
	_, err = provider.UpTo(ctx, 36)
	require.NoError(t, err)
	seedBackgroundProcessSessions(ctx, t, db)

	_, err = db.ExecContext(ctx, `INSERT INTO background_processes (
		id, session_id, root_session_id, tool_call_id, output_path,
		deadline_at, created_at, advertised_at, state
	) VALUES (
		'bgp_running', 1, 1, 'call_1', '/tmp/out/1.output',
		'2026-09-09 01:00:00', '2026-09-09 00:00:00', '2026-09-09 00:00:10', 'running'
	)`)
	require.NoError(t, err)

	_, err = provider.Up(ctx)
	require.NoError(t, err)

	var state string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT state FROM background_processes WHERE id = 'bgp_running'`,
	).Scan(&state))
	assert.Equal(t, "running", state)

	_, err = db.ExecContext(ctx, `UPDATE background_processes
		SET host_intent = 'agent_cancelled', state = 'cancelled', finished_at = '2026-09-09 00:05:00'
		WHERE id = 'bgp_running'`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `UPDATE background_processes
		SET host_intent = 'unknown_intent' WHERE id = 'bgp_running'`)
	require.Error(t, err)
}
