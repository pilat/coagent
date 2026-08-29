package migrate

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrate_ModelInputGenerationBackfill(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v27-gen.db")
	db, err := OpenDB(context.Background(), dbPath)
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
	provider := newProvider(t, db)
	_, err = provider.UpTo(ctx, 27)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx,
		`INSERT INTO projects (id, work_dir, name) VALUES (1, '/tmp/p', 'p')`)
	require.NoError(t, err)
	// Session 1 has history; session 2 is empty.
	_, err = db.ExecContext(ctx,
		`INSERT INTO sessions (id, project_id, model, agent_type) VALUES (1, 1, 'm', 'build')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO sessions (id, project_id, model, agent_type) VALUES (2, 1, 'm', 'build')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO messages (id, session_id, role, content) VALUES (10, 1, 'user', 'old input')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO messages (id, session_id, role, content) VALUES (11, 1, 'assistant', 'old narration')`)
	require.NoError(t, err)

	_, err = provider.Up(ctx)
	require.NoError(t, err)

	var gen1 int64
	var boundary1 sql.NullInt64
	require.NoError(t, db.QueryRow(`SELECT model_input_generation, model_input_boundary
		FROM sessions WHERE id = 1`).Scan(&gen1, &boundary1))
	assert.Equal(t, int64(1), gen1)
	require.True(t, boundary1.Valid)
	assert.Equal(t, int64(11), boundary1.Int64)

	var gen2 int64
	var boundary2 sql.NullInt64
	require.NoError(t, db.QueryRow(`SELECT model_input_generation, model_input_boundary
		FROM sessions WHERE id = 2`).Scan(&gen2, &boundary2))
	assert.Equal(t, int64(0), gen2)
	assert.False(t, boundary2.Valid)
}
