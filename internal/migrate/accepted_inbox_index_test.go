package migrate

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrate_AcceptedInboxIndexPreservesExistingRows(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v22-inbox.db")
	db, err := OpenDB(context.Background(), dbPath)
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
	provider := newProvider(t, db)
	_, err = provider.UpTo(ctx, 22)
	require.NoError(t, err)
	require.False(t, indexExists(t, db, "session_inbox", "idx_session_inbox_accepted_session"))

	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, work_dir, name) VALUES (1, '/tmp/p', 'p')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO sessions (id, project_id, model, agent_type) VALUES (1, 1, 'm', 'build')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO messages (id, session_id, role, content) VALUES (1, 1, 'user', 'accepted')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO session_inbox
			(id, session_id, source, raw_content, received_at, state, resolved_at, accepted_message_id)
		VALUES (1, 1, 'user', 'accepted', 100, 'accepted', 100, 1)`)
	require.NoError(t, err)

	_, err = provider.Up(ctx)
	require.NoError(t, err)
	assert.True(t, indexExists(t, db, "session_inbox", "idx_session_inbox_accepted_session"))
	assert.Equal(t, 1, countRows(t, db, "session_inbox", "state = 'accepted'"))
}
