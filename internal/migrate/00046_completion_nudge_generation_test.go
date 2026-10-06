package migrate

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrate_46_CompletionNudgeGeneration(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "nudge-generation.db")
	db, err := OpenDB(ctx, dbPath)
	require.NoError(t, err)
	defer db.Close()
	provider := newProvider(t, db)
	_, err = provider.UpTo(ctx, 45)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, work_dir, name) VALUES (1, '/tmp/p', 'p')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO sessions (id, project_id, model, agent_type)
		VALUES (1, 1, 'm', 'build')`)
	require.NoError(t, err)
	_, err = provider.Up(ctx)
	require.NoError(t, err)
	assert.True(t, columnExists(t, db, "sessions", "completion_nudge_generation"))
	var generation sql.NullInt64
	require.NoError(
		t,
		db.QueryRowContext(ctx, `SELECT completion_nudge_generation FROM sessions WHERE id = 1`).Scan(&generation),
	)
	assert.False(t, generation.Valid)
	_, err = db.ExecContext(
		ctx,
		`UPDATE sessions SET completion_nudge_generation = model_input_generation WHERE id = 1`,
	)
	require.NoError(t, err)
	require.NoError(
		t,
		db.QueryRowContext(ctx, `SELECT completion_nudge_generation FROM sessions WHERE id = 1`).Scan(&generation),
	)
	assert.True(t, generation.Valid)
	assert.Zero(t, generation.Int64)
}
