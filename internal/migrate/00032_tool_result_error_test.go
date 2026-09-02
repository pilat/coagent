package migrate

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Migration 32 adds the typed tool-result error bit. Legacy tool rows read as
// false (column default), so pre-migration results keep their ordinary
// semantics; new rows carry the bit through the store codecs.
func TestMigrate_32_ToolResultErrorDefaultsLegacyRowsToFalse(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "tool-error.db")
	db, err := OpenDB(ctx, dbPath)
	require.NoError(t, err)
	defer db.Close()

	provider := newProvider(t, db)

	if _, err := provider.UpTo(ctx, 31); err != nil {
		t.Fatalf("migrate to 31: %v", err)
	}

	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, work_dir, name) VALUES (1, '/tmp/p', 'p')`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `
		INSERT INTO sessions (id, project_id, model, agent_type)
		VALUES (1, 1, 'm', 'build')`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `
		INSERT INTO messages (id, session_id, role, content, tool_call_id, tool_name, position)
		VALUES (1, 1, 'tool', 'legacy result', 'c1', 'read', 1)`)
	require.NoError(t, err)

	_, err = provider.Up(ctx)
	require.NoError(t, err)

	assert.True(t, columnExists(t, db, "messages", "tool_error"), "messages.tool_error must exist")

	var legacyBit bool
	require.NoError(t, db.QueryRowContext(
		ctx, `SELECT tool_error FROM messages WHERE id = 1`,
	).Scan(&legacyBit))
	assert.False(t, legacyBit, "a legacy row reads as an ordinary result")
}

func TestMigrate_32_ToolResultErrorRoundTripsOnFreshDB(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "tool-error-fresh.db")
	db, err := OpenDB(ctx, dbPath)
	require.NoError(t, err)
	defer db.Close()

	provider := newProvider(t, db)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, work_dir, name) VALUES (1, '/tmp/p', 'p')`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `
		INSERT INTO sessions (id, project_id, model, agent_type)
		VALUES (1, 1, 'm', 'build')`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `
		INSERT INTO messages (session_id, role, content, tool_call_id, tool_name, tool_error)
		VALUES
			(1, 'tool', 'typed failure', 'c1', 'batch', 1),
			(1, 'tool', 'ordinary result', 'c2', 'read', 0)`)
	require.NoError(t, err)

	rows, err := db.QueryContext(ctx,
		`SELECT tool_call_id, tool_error FROM messages WHERE session_id = 1 ORDER BY id`)
	require.NoError(t, err)
	defer rows.Close()

	got := map[string]bool{}

	for rows.Next() {
		var callID string
		var toolError bool
		require.NoError(t, rows.Scan(&callID, &toolError))
		got[callID] = toolError
	}

	require.NoError(t, rows.Err())
	assert.Equal(t, map[string]bool{"c1": true, "c2": false}, got)
}
