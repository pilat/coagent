package migrate

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A DB at version 20 with a root row carrying the accidental 'general' schema default
// plus a real subagent row: 00021 must move roots only.
func TestMigrate_RootAgentTypeOnExistingDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v20.db")
	db, err := OpenDB(context.Background(), dbPath)
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
	provider := newProvider(t, db)
	_, err = provider.UpTo(ctx, 20)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, work_dir, name) VALUES (1, '/tmp/p', 'p')`)
	require.NoError(t, err)

	// A root as the old daemon wrote it: agent_type omitted, so the column
	// default applied. A NULL-typed root is the same accident.
	_, err = db.ExecContext(ctx, `INSERT INTO sessions (id, project_id, model) VALUES (1, 1, 'm')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO sessions (id, project_id, model, agent_type) VALUES (2, 1, 'm', NULL)`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx,
		`INSERT INTO sessions (id, project_id, model, parent_id, root_id, agent_type)
		 VALUES (3, 1, 'm', 1, 1, 'general')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO sessions (id, project_id, model, parent_id, root_id, agent_type)
		 VALUES (4, 1, 'm', 1, 1, 'explore')`)
	require.NoError(t, err)

	require.Equal(t, "general", agentTypeOf(t, db, 1), "the bug is present before 00021")

	_, err = provider.Up(ctx)
	require.NoError(t, err)

	assert.Equal(t, "build", agentTypeOf(t, db, 1), "defaulted root corrected")
	assert.Equal(t, "build", agentTypeOf(t, db, 2), "NULL-typed root corrected")
	assert.Equal(t, "general", agentTypeOf(t, db, 3), "a real subagent keeps its type")
	assert.Equal(t, "explore", agentTypeOf(t, db, 4))
}

// TestMigrate_RootAgentTypeFreshDB applies the whole chain to an empty database:
// 00021 must be a harmless no-op there.
func TestMigrate_RootAgentTypeFreshDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "fresh21.db")
	db, err := OpenDB(context.Background(), dbPath)
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
	require.NoError(t, Run(ctx, db, dbPath))

	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&count))
	assert.Zero(t, count, "a fresh DB has no sessions to correct")
}

func agentTypeOf(t *testing.T, db *sql.DB, id int64) string {
	t.Helper()

	var agentType sql.NullString
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT agent_type FROM sessions WHERE id = ?`, id).Scan(&agentType))

	return agentType.String
}
