package migrate

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/migrations"
)

func newProvider(t *testing.T, db *sql.DB) *goose.Provider {
	t.Helper()

	p, err := goose.NewProvider(
		goose.DialectSQLite3,
		db,
		migrations.FS,
		goose.WithGoMigrations(legacyMigrations()...),
		goose.WithLogger(goose.NopLogger()),
	)
	require.NoError(t, err)

	return p
}

func columnExists(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()

	rows, err := db.QueryContext(context.Background(), "PRAGMA table_info("+table+")")
	require.NoError(t, err)
	defer rows.Close()

	for rows.Next() {
		var (
			cid        int
			name, typ  string
			notNull    int
			dflt       sql.NullString
			primaryKey int
		)
		require.NoError(t, rows.Scan(&cid, &name, &typ, &notNull, &dflt, &primaryKey))

		if name == column {
			return true
		}
	}

	require.NoError(t, rows.Err())

	return false
}

func tableExists(t *testing.T, db *sql.DB, table string) bool {
	t.Helper()

	var name string
	err := db.QueryRowContext(
		context.Background(),
		"SELECT name FROM sqlite_master WHERE type IN ('table','view') AND name = ?",
		table,
	).Scan(&name)
	if err == sql.ErrNoRows {
		return false
	}

	require.NoError(t, err)

	return name == table
}

// TestMigrate_FreshDB applies all migrations to an empty database.
func TestMigrate_FreshDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "fresh.db")
	db, err := OpenDB(context.Background(), dbPath)
	require.NoError(t, err)
	defer db.Close()

	require.NoError(t, Run(context.Background(), db, dbPath))

	assert.True(t, columnExists(t, db, "sessions", "root_id"), "sessions.root_id must exist")
	assert.True(t, columnExists(t, db, "subagent_links", "task_call_id"), "subagent_links must exist")
	assert.True(t, columnExists(t, db, "subagent_links", "result"), "subagent_links.result must exist")
	assert.True(t, columnExists(t, db, "subagent_links", "outcome"), "subagent_links.outcome must exist")
	assert.False(t, columnExists(t, db, "subagent_links", "timeout_sec"), "subagent_links.timeout_sec must be dropped")
	assert.True(t, columnExists(t, db, "messages", "position"), "messages.position must exist")
}

// TestMigrate_SubagentLinksDropTimeout brings a DB to version 30 with a live
// foreground link carrying a nonzero legacy timeout, applies 00031, and verifies
// the column is dropped while every other link field survives unchanged.
func TestMigrate_SubagentLinksDropTimeout(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v30timeout.db")
	db, err := OpenDB(context.Background(), dbPath)
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()

	provider := newProvider(t, db)
	_, err = provider.UpTo(ctx, 30)
	require.NoError(t, err)

	require.True(t, columnExists(t, db, "subagent_links", "timeout_sec"), "timeout_sec must exist at v30")

	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, work_dir, name) VALUES (1, '/tmp/p', 'p')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO sessions (id, project_id, agent_type) VALUES (1, 1, 'build')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO sessions (id, project_id, parent_id, root_id, agent_type) VALUES (2, 1, 1, 1, 'general')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO subagent_links (parent_id, child_id, task_call_id, blocking, depth, state, timeout_sec, created_at)
		 VALUES (1, 2, 'call-1', 1, 1, 'spawned', 300, 100)`)
	require.NoError(t, err)

	_, err = provider.Up(ctx)
	require.NoError(t, err)

	assert.False(t, columnExists(t, db, "subagent_links", "timeout_sec"), "timeout_sec dropped by 00031")

	var (
		taskCallID string
		blocking   bool
		depth      int
		state      string
		createdAt  int64
	)
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT task_call_id, blocking, depth, state, created_at FROM subagent_links WHERE child_id = 2`,
	).Scan(&taskCallID, &blocking, &depth, &state, &createdAt))
	assert.Equal(t, "call-1", taskCallID)
	assert.True(t, blocking, "blocking survives the drop")
	assert.Equal(t, 1, depth)
	assert.Equal(t, "spawned", state)
	assert.Equal(t, int64(100), createdAt)
}

// TestMigrate_SubagentResultColumns brings a DB to version 9 (subagent_links
// without result/outcome) with a live link row, then applies 00010 and verifies
// the additive columns land with their ” default on the pre-existing row.
func TestMigrate_SubagentResultColumns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v9.db")
	db, err := OpenDB(context.Background(), dbPath)
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()

	provider := newProvider(t, db)
	_, err = provider.UpTo(ctx, 9)
	require.NoError(t, err)

	require.False(t, columnExists(t, db, "subagent_links", "result"), "result must not exist at v9")

	// Seed a link row as a real daemon at v9 would have.
	_, err = db.ExecContext(ctx,
		`INSERT INTO subagent_links (parent_id, child_id, task_call_id, created_at) VALUES (?, ?, ?, ?)`,
		1, 2, "call-x", 100)
	require.NoError(t, err)

	_, err = provider.Up(ctx)
	require.NoError(t, err)

	assert.True(t, columnExists(t, db, "subagent_links", "result"), "result added by 00010")
	assert.True(t, columnExists(t, db, "subagent_links", "outcome"), "outcome added by 00010")

	var result, outcome string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT result, outcome FROM subagent_links WHERE child_id = ?`, 2).Scan(&result, &outcome))
	assert.Empty(t, result, "pre-existing row defaults result to ''")
	assert.Empty(t, outcome, "pre-existing row defaults outcome to ''")
}

func TestMigrate_MessagePositionInitializesExistingRows(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v10.db")
	db, err := OpenDB(context.Background(), dbPath)
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
	provider := newProvider(t, db)
	_, err = provider.UpTo(ctx, 10)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, work_dir, name, node) VALUES (1, '/tmp/p', 'p', 'local')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO sessions (id, project_id) VALUES (1, 1)`)
	require.NoError(t, err)
	result, err := db.ExecContext(ctx, `INSERT INTO messages (session_id, role, content) VALUES (1, 'user', 'task')`)
	require.NoError(t, err)
	messageID, err := result.LastInsertId()
	require.NoError(t, err)

	_, err = provider.Up(ctx)
	require.NoError(t, err)

	var position int64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT position FROM messages WHERE id = ?`, messageID).Scan(&position))
	assert.Equal(t, messageID, position)
}

// TestMigrate_ContextLog brings a DB to version 11 with a seeded extraction row,
// then applies 00012 and verifies messages.cleared_at lands and the extraction
// tables are dropped.
func TestMigrate_ContextLog(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v11.db")
	db, err := OpenDB(context.Background(), dbPath)
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
	provider := newProvider(t, db)
	_, err = provider.UpTo(ctx, 11)
	require.NoError(t, err)

	require.False(t, columnExists(t, db, "messages", "cleared_at"), "cleared_at must not exist at v11")

	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, work_dir, name, node) VALUES (1, '/tmp/p', 'p', 'local')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO sessions (id, project_id) VALUES (1, 1)`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO extractions (project_id, session_id, text) VALUES (1, 1, 'x')`)
	require.NoError(t, err)

	_, err = provider.UpTo(ctx, 12)
	require.NoError(t, err)

	assert.True(t, columnExists(t, db, "messages", "cleared_at"), "cleared_at added by 00012")

	_, err = provider.Up(ctx)
	require.NoError(t, err)

	assert.False(t, columnExists(t, db, "messages", "cleared_at"), "cleared_at dropped again by 00028")
	assert.False(t, tableExists(t, db, "extractions"), "extractions dropped by 00012")
	assert.False(t, tableExists(t, db, "extraction_chunks"), "extraction_chunks dropped by 00012")
	assert.False(t, tableExists(t, db, "memory_meta"), "memory_meta dropped by 00012")
	assert.True(t, tableExists(t, db, "memories"), "curated memories table must survive")
}

// TestMigrate_ScheduleFresh brings a DB to version 12 with a live schedule row,
// then applies 00013 and verifies schedules.fresh lands defaulting to 0.
func TestMigrate_ScheduleFresh(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v12.db")
	db, err := OpenDB(context.Background(), dbPath)
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
	provider := newProvider(t, db)
	_, err = provider.UpTo(ctx, 12)
	require.NoError(t, err)

	require.False(t, columnExists(t, db, "schedules", "fresh"), "fresh must not exist at v12")

	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, work_dir, name, node) VALUES (1, '/tmp/p', 'p', 'local')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO sessions (id, project_id) VALUES (1, 1)`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO schedules (id, session_id, cron_expr, input_message) VALUES (1, 1, '0 9 * * *', 'existing')`)
	require.NoError(t, err)

	_, err = provider.Up(ctx)
	require.NoError(t, err)

	assert.True(t, columnExists(t, db, "schedules", "fresh"), "fresh added by 00013")

	var fresh int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT fresh FROM schedules WHERE id = 1`).Scan(&fresh))
	assert.Equal(t, 0, fresh, "pre-existing schedules default to fresh=0")
}

func TestMigrateDropsProjectsNodeColumn(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v14fresh.db")
	db, err := OpenDB(context.Background(), dbPath)
	require.NoError(t, err)
	defer db.Close()

	require.NoError(t, Run(context.Background(), db, dbPath))

	assert.False(t, columnExists(t, db, "projects", "node"), "projects.node dropped by 00014")

	unique, found := indexUnique(t, db, "projects", "idx_projects_workdir")
	assert.True(t, found, "idx_projects_workdir must exist")
	assert.True(t, unique, "idx_projects_workdir must be unique")
	assert.False(t, indexExists(t, db, "projects", "idx_projects_workdir_node"), "composite index dropped")
}

func TestMigrateDeletesFollowerProjects(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v13.db")
	db, err := OpenDB(context.Background(), dbPath)
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
	provider := newProvider(t, db)
	_, err = provider.UpTo(ctx, 13)
	require.NoError(t, err)

	seedProjectTree(t, db, 1, "local", "/tmp/local", 1, 2)
	seedProjectTree(t, db, 2, "docker", "/", 3, 4)

	_, err = provider.Up(ctx)
	require.NoError(t, err)

	assert.Equal(t, 1, countRows(t, db, "projects", ""))
	assert.Equal(t, 1, countRows(t, db, "projects", "id = 1"))
	assert.Equal(t, 2, countRows(t, db, "sessions", ""))
	assert.Equal(t, 0, countRows(t, db, "sessions", "id IN (3, 4)"))
	assert.Equal(t, 2, countRows(t, db, "messages", ""))
	assert.Equal(t, 0, countRows(t, db, "messages", "session_id IN (3, 4)"))
	assert.Equal(t, 1, countRows(t, db, "schedules", ""))
	assert.Equal(t, 0, countRows(t, db, "schedules", "session_id IN (3, 4)"))
	assert.Equal(t, 1, countRows(t, db, "memories", ""))
	assert.Equal(t, 0, countRows(t, db, "memories", "project_id = 2"))
	// subagent_links has no FK — a mis-ordered delete orphans it silently.
	assert.Equal(t, 1, countRows(t, db, "subagent_links", ""))
	assert.Equal(t, 0, countRows(t, db, "subagent_links", "parent_id = 3 OR child_id = 4"))
}

func TestMigrateStripsLocalModelPrefix(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v13models.db")
	db, err := OpenDB(context.Background(), dbPath)
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
	provider := newProvider(t, db)
	_, err = provider.UpTo(ctx, 13)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx,
		`INSERT INTO projects (id, work_dir, name, node) VALUES (1, '/tmp/p', 'p', 'local')`)
	require.NoError(t, err)

	models := []string{"local:z-ai/glm-5.2", "anthropic/claude-sonnet-4.6", ""}
	for i, m := range models {
		_, err = db.ExecContext(ctx, `INSERT INTO sessions (id, project_id, model) VALUES (?, 1, ?)`, i+1, m)
		require.NoError(t, err)
	}

	_, err = provider.Up(ctx)
	require.NoError(t, err)

	want := []string{"z-ai/glm-5.2", "anthropic/claude-sonnet-4.6", ""}
	for i, expected := range want {
		var got string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT model FROM sessions WHERE id = ?`, i+1).Scan(&got))
		assert.Equal(t, expected, got)
	}
}

func TestMigrateIsNoOpWithoutFollowerProjects(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v13local.db")
	db, err := OpenDB(context.Background(), dbPath)
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
	provider := newProvider(t, db)
	_, err = provider.UpTo(ctx, 13)
	require.NoError(t, err)

	seedProjectTree(t, db, 1, "local", "/tmp/a", 1, 2)
	seedProjectTree(t, db, 2, "local", "/tmp/b", 3, 4)

	_, err = provider.Up(ctx)
	require.NoError(t, err)

	assert.Equal(t, 2, countRows(t, db, "projects", ""))
	assert.Equal(t, 4, countRows(t, db, "sessions", ""))
	assert.Equal(t, 4, countRows(t, db, "messages", ""))
	assert.Equal(t, 2, countRows(t, db, "schedules", ""))
	assert.Equal(t, 2, countRows(t, db, "memories", ""))
	assert.Equal(t, 2, countRows(t, db, "subagent_links", ""))
	assert.False(t, columnExists(t, db, "projects", "node"))
}

// seedProjectTree inserts a project with two sessions and one row in every table
// that hangs off them, as a pre-00014 daemon would have.
func seedProjectTree(t *testing.T, db *sql.DB, projectID int64, node, workDir string, parentID, childID int64) {
	t.Helper()

	ctx := context.Background()

	_, err := db.ExecContext(ctx, `INSERT INTO projects (id, work_dir, name, node) VALUES (?, ?, ?, ?)`,
		projectID, workDir, "p", node)
	require.NoError(t, err)

	for _, sid := range []int64{parentID, childID} {
		_, err = db.ExecContext(ctx, `INSERT INTO sessions (id, project_id, model) VALUES (?, ?, 'm')`, sid, projectID)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `INSERT INTO messages (session_id, role, content) VALUES (?, 'user', 'task')`, sid)
		require.NoError(t, err)
	}

	_, err = db.ExecContext(ctx,
		`INSERT INTO schedules (session_id, cron_expr, input_message) VALUES (?, '0 9 * * *', 'wake')`, parentID)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `INSERT INTO memories (project_id, text) VALUES (?, 'note')`, projectID)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx,
		`INSERT INTO subagent_links (parent_id, child_id, task_call_id, created_at) VALUES (?, ?, 'call', 100)`,
		parentID, childID)
	require.NoError(t, err)
}

func countRows(t *testing.T, db *sql.DB, table, where string) int {
	t.Helper()

	query := "SELECT COUNT(*) FROM " + table
	if where != "" {
		query += " WHERE " + where
	}

	var n int
	require.NoError(t, db.QueryRowContext(context.Background(), query).Scan(&n))

	return n
}

// indexUnique reports whether the named index on table is unique, and whether it exists at all.
func indexUnique(t *testing.T, db *sql.DB, table, index string) (bool, bool) {
	t.Helper()

	rows, err := db.QueryContext(context.Background(), "PRAGMA index_list("+table+")")
	require.NoError(t, err)
	defer rows.Close()

	for rows.Next() {
		var (
			seq            int
			name, origin   string
			isUnique, part int
		)
		require.NoError(t, rows.Scan(&seq, &name, &isUnique, &origin, &part))

		if name == index {
			return isUnique == 1, true
		}
	}

	require.NoError(t, rows.Err())

	return false, false
}

func indexExists(t *testing.T, db *sql.DB, table, index string) bool {
	t.Helper()

	_, found := indexUnique(t, db, table, index)

	return found
}

// TestMigrate_ExistingDBUpgrade simulates a daemon DB already at version 8 with
// live data, then applies 00009 and verifies the additive schema change is clean.
func TestMigrate_ExistingDBUpgrade(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "existing.db")
	db, err := OpenDB(context.Background(), dbPath)
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()

	// Bring the DB to version 8 (pre-subagent-links state).
	provider := newProvider(t, db)
	_, err = provider.UpTo(ctx, 8)
	require.NoError(t, err)

	require.False(t, columnExists(t, db, "sessions", "root_id"), "root_id must not exist at v8")

	// Seed a project + a session row, as a real daemon would have.
	res, err := db.ExecContext(ctx, `INSERT INTO projects (work_dir, name, node) VALUES (?, ?, ?)`,
		"/tmp/p", "p", "local")
	require.NoError(t, err)
	pid, err := res.LastInsertId()
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `INSERT INTO sessions (project_id, model) VALUES (?, ?)`, pid, "m")
	require.NoError(t, err)

	// Apply the remaining migration (00009).
	_, err = provider.Up(ctx)
	require.NoError(t, err)

	assert.True(t, columnExists(t, db, "sessions", "root_id"), "root_id added by 00009")
	assert.True(t, columnExists(t, db, "subagent_links", "child_id"), "subagent_links added by 00009")

	// Pre-existing row gets the default root_id = 0.
	var rootID int64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT root_id FROM sessions WHERE project_id = ?`, pid).Scan(&rootID))
	assert.Equal(t, int64(0), rootID)
}

// TestMigrate_ZeroHistoricalSummaryCost brings a DB to version 14 with a compacted
// original + a summary row carrying the old rolled-up cost, applies 00015, and
// verifies the summary cost is zeroed so a lifetime tree-sum counts the original once.
func TestMigrate_ZeroHistoricalSummaryCost(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v14summary.db")
	db, err := OpenDB(context.Background(), dbPath)
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
	provider := newProvider(t, db)
	_, err = provider.UpTo(ctx, 14)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, work_dir, name) VALUES (1, '/tmp/p', 'p')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO sessions (id, project_id) VALUES (1, 1)`)
	require.NoError(t, err)

	// The original the summary replaced still lives here (compacted), keeping cost 3.0.
	_, err = db.ExecContext(ctx,
		`INSERT INTO messages (id, session_id, role, content, cost_usd, compacted_at)
		 VALUES (1, 1, 'assistant', 'work', 3.0, '2026-01-01 00:00:00')`)
	require.NoError(t, err)
	// The summary row carries the rolled-up 3.0 — the double-count to be zeroed.
	_, err = db.ExecContext(ctx,
		`INSERT INTO messages (id, session_id, role, content, cost_usd)
		 VALUES (2, 1, 'user', '[CONTEXT SUMMARY - previous work condensed] brief', 3.0)`)
	require.NoError(t, err)
	// A non-summary user message must be left untouched.
	_, err = db.ExecContext(ctx,
		`INSERT INTO messages (id, session_id, role, content, cost_usd) VALUES (3, 1, 'user', 'real task', 1.0)`)
	require.NoError(t, err)

	_, err = provider.Up(ctx)
	require.NoError(t, err)

	var summaryCost, otherCost, treeSum float64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT cost_usd FROM messages WHERE id = 2`).Scan(&summaryCost))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT cost_usd FROM messages WHERE id = 3`).Scan(&otherCost))
	require.NoError(t,
		db.QueryRowContext(ctx, `SELECT COALESCE(SUM(cost_usd), 0) FROM messages WHERE session_id = 1`).Scan(&treeSum))

	assert.InDelta(t, 0.0, summaryCost, 1e-9, "summary rollup zeroed")
	assert.InDelta(t, 1.0, otherCost, 1e-9, "non-summary user message untouched")
	assert.InDelta(t, 4.0, treeSum, 1e-9, "original 3.0 counted once + real task 1.0, not 7.0")
}

// TestMigrate_MessageAttachments brings a DB to version 25 (the Go
// manager-outbox backfill; SQL slots jump 00024 → 00026) with live message rows,
// applies 00026, and verifies messages.attachments lands as a nullable
// NULL-defaulting column on the pre-existing rows.
func TestMigrate_MessageAttachments(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v25attachments.db")
	db, err := OpenDB(context.Background(), dbPath)
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
	provider := newProvider(t, db)
	_, err = provider.UpTo(ctx, 25)
	require.NoError(t, err)

	require.False(t, columnExists(t, db, "messages", "attachments"), "attachments must not exist at v25")

	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, work_dir, name) VALUES (1, '/tmp/p', 'p')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO sessions (id, project_id, agent_type) VALUES (1, 1, 'general')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO messages (session_id, role, content) VALUES (1, 'user', 'task')`)
	require.NoError(t, err)

	_, err = provider.Up(ctx)
	require.NoError(t, err)

	assert.True(t, columnExists(t, db, "messages", "attachments"), "attachments added by 00026")

	var attachments sql.NullString
	require.NoError(t,
		db.QueryRowContext(ctx, `SELECT attachments FROM messages WHERE session_id = 1`).Scan(&attachments))
	assert.False(t, attachments.Valid, "pre-existing row defaults attachments to NULL")
}
