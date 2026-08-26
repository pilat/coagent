package mcpstore

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/migrate"
)

func newTestStore(t *testing.T) (Store, *sql.DB, int64) {
	t.Helper()

	db := migratedDB(t)

	res, err := db.ExecContext(context.Background(),
		`INSERT INTO projects (work_dir, name) VALUES (?, ?)`, t.TempDir(), "test")
	require.NoError(t, err)

	projectID, err := res.LastInsertId()
	require.NoError(t, err)

	return NewStore(db), db, projectID
}

func migratedDB(t *testing.T) *sql.DB {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "test.db")

	db, err := migrate.OpenDB(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(context.Background(), db, dbPath))

	return db
}

func def(name string) ServerDef {
	return ServerDef{
		Name:    name,
		Command: "npx",
		Args:    []string{"-y", name},
		Env:     map[string]string{"TOKEN": "${" + name + "_TOKEN}"},
		Enabled: true,
	}
}

func names(defs []ServerDef) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.Name)
	}

	return out
}

func TestMigrationCreatesTableAndPartialIndexes(t *testing.T) {
	db := migratedDB(t)
	ctx := context.Background()

	var count int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='mcp_servers'`).Scan(&count))
	assert.Equal(t, 1, count)

	for _, index := range []string{"idx_mcp_servers_project_name", "idx_mcp_servers_global_name"} {
		var sqlText string
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT sql FROM sqlite_master WHERE type='index' AND name=?`, index).Scan(&sqlText))
		assert.Contains(t, sqlText, "WHERE project_id IS")
	}
}

// Rewinding every version from 00016 up reproduces an existing database; goose
// refuses to fill a gap below the recorded version, so they go together.
func TestMigrationsApplyToAnExistingDatabase(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "existing.db")

	db, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(ctx, db, dbPath))

	// Seed a row the migration must not disturb.
	_, err = db.ExecContext(ctx, `INSERT INTO projects (work_dir, name) VALUES (?, ?)`, t.TempDir(), "kept")
	require.NoError(t, err)

	for _, stmt := range []string{
		`DROP TABLE mcp_servers`,
		`ALTER TABLE messages DROP COLUMN reasoning_raw`,
		`DROP TABLE session_inbox`,
		`ALTER TABLE subagent_links DROP COLUMN activation_seq`,
		`DROP TABLE session_deliveries`,
		`DROP TABLE session_outbox`,
		`DROP TABLE manager_bindings`,
		`DELETE FROM goose_db_version WHERE version_id IN (16, 17, 18, 19, 20, 21, 22, 23, 24, 25)`,
	} {
		_, err = db.ExecContext(ctx, stmt)
		require.NoError(t, err, stmt)
	}

	require.Equal(t, 0, countMCPServersTable(t, db), "the table must be absent before 00016 re-applies")

	require.NoError(t, migrate.Run(ctx, db, dbPath))
	assert.Equal(t, 1, countMCPServersTable(t, db))

	for _, index := range []string{"idx_mcp_servers_project_name", "idx_mcp_servers_global_name"} {
		var count int
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?`, index).Scan(&count))
		assert.Equal(t, 1, count, index)
	}

	var kept int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM projects WHERE name = 'kept'`).Scan(&kept))
	assert.Equal(t, 1, kept, "existing data survives the migration")
}

func countMCPServersTable(t *testing.T, db *sql.DB) int {
	t.Helper()

	var count int
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='mcp_servers'`).Scan(&count))

	return count
}

func TestAddAndListRoundTripsJSON(t *testing.T) {
	ctx := context.Background()
	store, _, projectID := newTestStore(t)

	want := ServerDef{
		Name:    "tavily",
		Command: "npx",
		Args:    []string{"-y", "tavily-mcp@latest"},
		Env:     map[string]string{"TAVILY_API_KEY": "${TAVILY_API_KEY}"},
		Enabled: true,
	}
	require.NoError(t, store.Add(ctx, &projectID, want))

	got, err := store.ListForProject(ctx, projectID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, want, got[0])
}

func TestAddDefaultsEmptyArgsAndEnv(t *testing.T) {
	ctx := context.Background()
	store, _, projectID := newTestStore(t)

	require.NoError(t, store.Add(ctx, &projectID, ServerDef{Name: "bare", Command: "run", Enabled: true}))

	got, err := store.ListForProject(ctx, projectID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Empty(t, got[0].Args)
	assert.Empty(t, got[0].Env)
}

func TestAddRejectsDuplicatesWithinAScope(t *testing.T) {
	ctx := context.Background()
	store, _, projectID := newTestStore(t)

	require.NoError(t, store.Add(ctx, nil, def("dup")))
	require.ErrorIs(t, store.Add(ctx, nil, def("dup")), ErrDuplicate)

	require.NoError(
		t,
		store.Add(ctx, &projectID, def("dup")),
		"the same name in another scope is an override, not a clash",
	)
	require.ErrorIs(t, store.Add(ctx, &projectID, def("dup")), ErrDuplicate)
}

func TestListForProjectMergesScopes(t *testing.T) {
	ctx := context.Background()
	store, _, projectID := newTestStore(t)

	require.NoError(t, store.Add(ctx, nil, def("shared")))
	require.NoError(t, store.Add(ctx, nil, def("global-only")))
	require.NoError(t, store.Add(ctx, &projectID, def("project-only")))

	override := def("shared")
	override.Command = "project-command"
	require.NoError(t, store.Add(ctx, &projectID, override))

	got, err := store.ListForProject(ctx, projectID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"shared", "global-only", "project-only"}, names(got))

	for _, d := range got {
		if d.Name == "shared" {
			assert.Equal(t, "project-command", d.Command, "the project row wins")
		}
	}
}

func TestListForProjectSkipsDisabledRows(t *testing.T) {
	ctx := context.Background()
	store, _, projectID := newTestStore(t)

	require.NoError(t, store.Add(ctx, nil, def("off-global")))
	require.NoError(t, store.Add(ctx, &projectID, def("off-project")))
	require.NoError(t, store.SetEnabled(ctx, nil, "off-global", false))
	require.NoError(t, store.SetEnabled(ctx, &projectID, "off-project", false))

	got, err := store.ListForProject(ctx, projectID)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// Disabling the project's own row means "not here", so it must not fall through
// to the global server of the same name.
func TestDisabledProjectRowShadowsTheGlobal(t *testing.T) {
	ctx := context.Background()
	store, _, projectID := newTestStore(t)

	require.NoError(t, store.Add(ctx, nil, def("shared")))
	require.NoError(t, store.Add(ctx, &projectID, def("shared")))
	require.NoError(t, store.SetEnabled(ctx, &projectID, "shared", false))

	got, err := store.ListForProject(ctx, projectID)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestListAllShowsDisabledRowsPerScope(t *testing.T) {
	ctx := context.Background()
	store, _, projectID := newTestStore(t)

	require.NoError(t, store.Add(ctx, nil, def("g1")))
	require.NoError(t, store.Add(ctx, &projectID, def("p1")))
	require.NoError(t, store.SetEnabled(ctx, &projectID, "p1", false))

	globals, project, err := store.ListAll(ctx, projectID)
	require.NoError(t, err)
	assert.Equal(t, []string{"g1"}, names(globals))
	require.Len(t, project, 1)
	assert.False(t, project[0].Enabled)
}

func TestRemove(t *testing.T) {
	ctx := context.Background()
	store, _, projectID := newTestStore(t)

	require.NoError(t, store.Add(ctx, &projectID, def("gone")))
	require.NoError(t, store.Remove(ctx, &projectID, "gone"))

	got, err := store.ListForProject(ctx, projectID)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestMutationsOnMissingNamesReportTheScope(t *testing.T) {
	ctx := context.Background()
	store, _, projectID := newTestStore(t)

	require.NoError(t, store.Add(ctx, nil, def("only-global")))

	err := store.Remove(ctx, &projectID, "only-global")
	require.ErrorIs(t, err, ErrNotFound)
	assert.Contains(t, err.Error(), "exists in global scope")

	err = store.SetEnabled(ctx, &projectID, "only-global", false)
	require.ErrorIs(t, err, ErrNotFound)
	assert.Contains(t, err.Error(), "exists in global scope")

	err = store.Remove(ctx, nil, "never-existed")
	require.ErrorIs(t, err, ErrNotFound)
	assert.NotContains(t, err.Error(), "exists in")
}

func TestGlobalMutationFindsProjectScopeHint(t *testing.T) {
	ctx := context.Background()
	store, _, projectID := newTestStore(t)

	require.NoError(t, store.Add(ctx, &projectID, def("only-project")))

	err := store.Remove(ctx, nil, "only-project")
	require.ErrorIs(t, err, ErrNotFound)
	assert.Contains(t, err.Error(), "exists in project scope")
}
