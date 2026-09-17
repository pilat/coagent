package migrate

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
)

// seedLegacyCLIGraph stages the complete old CLI world at version 39: the
// sys:coagent project, its root and child, and one row in every table the
// migration must delete. Unrelated project data sits beside it and must
// survive byte-for-byte.
func seedLegacyCLIGraph(ctx context.Context, t *testing.T, db *sql.DB) (int64, int64) {
	t.Helper()

	exec := func(query string, args ...any) {
		t.Helper()
		_, err := db.ExecContext(ctx, query, args...)
		require.NoError(t, err, query)
	}

	exec(`INSERT INTO projects (id, work_dir, name) VALUES (1, '/tmp/sys', 'sys:coagent')`)
	exec(`INSERT INTO projects (id, work_dir, name) VALUES (2, '/tmp/keep', 'keep')`)
	cliProjectID, otherProjectID := int64(1), int64(2)

	// The old CLI root (ownerless, channel=cli) plus one child; the kept
	// project gets its own root.
	exec(`INSERT INTO sessions (id, project_id, model, agent_type, attributes, created_at, updated_at)
		VALUES (10, 1, 'm', 'build', '{"channel":"cli"}', '2026-01-01 00:00:00', '2026-01-01 00:00:00')`)
	exec(`INSERT INTO sessions (id, project_id, model, agent_type, parent_id, root_id, created_at, updated_at)
		VALUES (11, 1, 'm', 'general', 10, 10, '2026-01-01 00:00:00', '2026-01-01 00:00:00')`)
	exec(`INSERT INTO sessions (id, project_id, model, agent_type, created_at, updated_at)
		VALUES (20, 2, 'm', 'build', '2026-01-01 00:00:00', '2026-01-01 00:00:00')`)

	exec(`INSERT INTO messages (session_id, role, content) VALUES (10, 'user', 'old task')`)
	exec(`INSERT INTO messages (session_id, role, content) VALUES (20, 'user', 'keep task')`)

	exec(`INSERT INTO schedules (session_id, cron_expr, input_message)
		VALUES (10, '0 9 * * *', 'old wake')`)
	exec(`INSERT INTO schedules (session_id, cron_expr, input_message)
		VALUES (20, '0 9 * * *', 'keep wake')`)

	exec(`INSERT INTO session_inbox (session_id, source, raw_content, received_at)
		VALUES (10, 'user', 'pending old input', '2026-01-01 00:00:00')`)
	exec(`INSERT INTO session_inbox (session_id, source, raw_content, received_at)
		VALUES (20, 'user', 'pending keep input', '2026-01-01 00:00:00')`)

	exec(`INSERT INTO session_outbox (session_id, type, content, attributes, created_at)
		VALUES (10, 'message_persistent', 'old answer', '{"manager_id":"cli"}', datetime('now'))`)
	exec(`INSERT INTO session_outbox (session_id, type, content, attributes, created_at)
		VALUES (20, 'message_persistent', 'keep answer', '{"manager_id":"tg"}', datetime('now'))`)

	exec(`INSERT INTO session_deliveries (session_id, delivery_id, kind, fingerprint, delivered_at)
		VALUES (10, 'd1', 'tool_notification', 'fp', '2026-01-01 00:00:00')`)
	exec(`INSERT INTO session_deliveries (session_id, delivery_id, kind, fingerprint, delivered_at)
		VALUES (20, 'd2', 'tool_notification', 'fp', '2026-01-01 00:00:00')`)

	exec(`INSERT INTO session_tool_activations (input_id, session_id, tool_id, command, state, created_at)
		VALUES (1, 10, 'config_edit', '/config', 'pending', datetime('now'))`)

	exec(`INSERT INTO session_budgets (root_session_id, state, generation, armed_at, baseline_cost_usd, cost_limit_usd)
		VALUES (10, 'armed', 1, datetime('now'), 0, 2.0)`)
	exec(`INSERT INTO session_budgets (root_session_id, state, generation, armed_at, baseline_cost_usd, cost_limit_usd)
		VALUES (20, 'armed', 1, datetime('now'), 0, 2.0)`)

	exec(`INSERT INTO session_file_reads (session_id, path, mtime_unix_nano, size, hash, updated_at)
		VALUES (10, '/tmp/file', 1, 1, 'h', datetime('now'))`)
	exec(`INSERT INTO session_file_reads (session_id, path, mtime_unix_nano, size, hash, updated_at)
		VALUES (20, '/tmp/keep', 1, 1, 'h', datetime('now'))`)

	exec(`INSERT INTO background_processes (id, session_id, root_session_id, tool_call_id, output_path,
		deadline_at, created_at, host_intent, state)
		VALUES ('bgp_old', 10, 10, 'c1', '/tmp/out/old', datetime('now'), datetime('now'), '', 'running')`)
	exec(`INSERT INTO background_processes (id, session_id, root_session_id, tool_call_id, output_path,
		deadline_at, created_at, host_intent, state)
		VALUES ('bgp_keep', 20, 20, 'c2', '/tmp/out/keep', datetime('now'), datetime('now'), '', 'running')`)

	exec(`INSERT INTO subagent_links (parent_id, child_id, task_call_id, created_at)
		VALUES (10, 11, 'old-call', 100)`)

	exec(`INSERT INTO memories (project_id, text) VALUES (1, 'old note')`)
	exec(`INSERT INTO memories (project_id, text) VALUES (2, 'keep note')`)

	exec(`INSERT INTO mcp_servers (project_id, name, command, created_at, updated_at)
		VALUES (1, 'old-srv', 'run', datetime('now'), datetime('now'))`)
	exec(`INSERT INTO mcp_servers (project_id, name, command, created_at, updated_at)
		VALUES (2, 'keep-srv', 'run', datetime('now'), datetime('now'))`)

	exec(`INSERT INTO manager_bindings (manager_id, driver, attributes, created_at, updated_at)
		VALUES ('cli', 'cli', '{"local":true}', datetime('now'), datetime('now'))`)
	exec(`INSERT INTO manager_bindings (manager_id, driver, attributes, created_at, updated_at)
		VALUES ('tg', 'telegram', '{"bot_user_id":1,"chat_id":2,"topology":"group"}', datetime('now'), datetime('now'))`)

	return cliProjectID, otherProjectID
}

func TestMigrate_40_DeletesCLIGraphAndKeepsUnrelatedData(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "v39.db")
	db, err := OpenDB(ctx, dbPath)
	require.NoError(t, err)
	defer db.Close()

	provider := newProvider(t, db)
	_, err = provider.UpTo(ctx, 39)
	require.NoError(t, err)

	require.False(t, columnExists(t, db, "projects", "hidden"), "hidden must not exist at v39")

	cliID, keepID := seedLegacyCLIGraph(ctx, t, db)

	_, err = provider.Up(ctx)
	require.NoError(t, err)

	require.True(t, columnExists(t, db, "projects", "hidden"))

	// Fresh rows default to visible.
	var hidden bool
	require.NoError(t, db.QueryRowContext(ctx, `SELECT hidden FROM projects WHERE id = ?`, keepID).Scan(&hidden))
	assert.False(t, hidden)

	assert.Equal(t, 0, countRows(t, db, "projects", "id = ?", cliID), "old CLI project deleted")
	assert.Equal(t, 0, countRows(t, db, "sessions", "project_id = ?", cliID), "old CLI sessions deleted")
	assert.Equal(t, 0, countRows(t, db, "messages", "session_id IN (10, 11)"))
	assert.Equal(t, 0, countRows(t, db, "schedules", "session_id = 10"))
	assert.Equal(t, 0, countRows(t, db, "session_inbox", "session_id = 10"))
	assert.Equal(t, 0, countRows(t, db, "session_outbox", "session_id = 10"))
	assert.Equal(t, 0, countRows(t, db, "session_deliveries", "session_id = 10"))
	assert.Equal(t, 0, countRows(t, db, "session_tool_activations", "session_id = 10"))
	assert.Equal(t, 0, countRows(t, db, "session_budgets", "root_session_id = 10"))
	assert.Equal(t, 0, countRows(t, db, "session_file_reads", "session_id = 10"))
	assert.Equal(t, 0, countRows(t, db, "background_processes", "id = 'bgp_old'"))
	assert.Equal(t, 0, countRows(t, db, "subagent_links", "parent_id = 10"))
	assert.Equal(t, 0, countRows(t, db, "memories", "project_id = ?", cliID))
	assert.Equal(t, 0, countRows(t, db, "mcp_servers", "project_id = ?", cliID))
	assert.Equal(t, 0, countRows(t, db, "manager_bindings", "manager_id = 'cli'"))

	// Unrelated project data survives.
	assert.Equal(t, 1, countRows(t, db, "projects", "id = ?", keepID))
	assert.Equal(t, 1, countRows(t, db, "sessions", "project_id = ?", keepID))
	assert.Equal(t, 1, countRows(t, db, "messages", "content = 'keep task'"))
	assert.Equal(t, 1, countRows(t, db, "schedules", "input_message = 'keep wake'"))
	assert.Equal(t, 1, countRows(t, db, "session_inbox", "raw_content = 'pending keep input'"))
	assert.Equal(t, 1, countRows(t, db, "session_outbox", "content = 'keep answer'"))
	assert.Equal(t, 1, countRows(t, db, "session_deliveries", "delivery_id = 'd2'"))
	assert.Equal(t, 1, countRows(t, db, "session_budgets", "root_session_id = 20"))
	assert.Equal(t, 1, countRows(t, db, "session_file_reads", "path = '/tmp/keep'"))
	assert.Equal(t, 1, countRows(t, db, "background_processes", "id = 'bgp_keep'"))
	assert.Equal(t, 1, countRows(t, db, "memories", "text = 'keep note'"))
	assert.Equal(t, 1, countRows(t, db, "mcp_servers", "project_id = ? AND name = 'keep-srv'", keepID))
	assert.Equal(t, 1, countRows(t, db, "manager_bindings", "manager_id = 'tg'"))
}

// Rerun safety: goose records the version only after the function returns, so
// a failure between commit and artifact cleanup must not corrupt a rerun.
func TestMigrate_40_RerunAfterCommitIsSafe(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "rerun.db")
	db, err := OpenDB(ctx, dbPath)
	require.NoError(t, err)
	defer db.Close()

	provider := newProvider(t, db)
	_, err = provider.UpTo(ctx, 39)
	require.NoError(t, err)

	seedLegacyCLIGraph(ctx, t, db)

	_, err = provider.Up(ctx)
	require.NoError(t, err)

	_, err = provider.Up(ctx)
	require.NoError(t, err, "second full run must be a no-op")
}

func TestMigrate_40_FreshDBAddsHiddenColumn(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "fresh.db")
	db, err := OpenDB(ctx, dbPath)
	require.NoError(t, err)
	defer db.Close()

	require.NoError(t, Run(ctx, db, dbPath))

	require.True(t, columnExists(t, db, "projects", "hidden"))
	assert.Equal(t, 0, countRows(t, db, "projects", "hidden = TRUE"), "fresh DB creates no legacy project")

	_, err = db.ExecContext(ctx, `INSERT INTO projects (work_dir, name) VALUES ('/tmp/x', 'x')`)
	require.NoError(t, err)
	var hidden bool
	require.NoError(t, db.QueryRowContext(ctx, `SELECT hidden FROM projects`).Scan(&hidden))
	assert.False(t, hidden, "ordinary projects default to visible")
}

func TestMigrate_40_RemovesExactProcessArtifactDirs(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	restore := coagenthome.Override(home)
	defer restore()
	require.NoError(t, os.MkdirAll(filepath.Join(home, coagenthome.DirName), 0o700))

	dbPath := filepath.Join(t.TempDir(), "artifacts.db")
	db, err := OpenDB(ctx, dbPath)
	require.NoError(t, err)
	defer db.Close()

	provider := newProvider(t, db)
	_, err = provider.UpTo(ctx, 39)
	require.NoError(t, err)

	cliID, keepID := seedLegacyCLIGraph(ctx, t, db)

	oldDir, err := coagenthome.ProcessProjectDir(cliID)
	require.NoError(t, err)
	keepDir, err := coagenthome.ProcessProjectDir(keepID)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(oldDir, 0o700))
	require.NoError(t, os.MkdirAll(keepDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(oldDir, "out"), []byte("x"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(keepDir, "out"), []byte("x"), 0o600))

	_, err = provider.Up(ctx)
	require.NoError(t, err)

	_, err = os.Stat(oldDir)
	assert.True(t, os.IsNotExist(err), "old project's process dir removed")
	_, err = os.Stat(keepDir)
	require.NoError(t, err, "unrelated project's process dir preserved")
}
