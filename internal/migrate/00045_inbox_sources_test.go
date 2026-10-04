package migrate

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrate_InboxSourcesCopiesLinksByColumnName(t *testing.T) {
	ctx := context.Background()
	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "v44-links.db"))
	require.NoError(t, err)
	defer db.Close()

	provider := newProvider(t, db)
	_, err = provider.UpTo(ctx, 44)
	require.NoError(t, err)

	// Older databases carry delivered_msg_id last, after a manual column restore.
	for _, stmt := range []string{
		`ALTER TABLE subagent_links DROP COLUMN delivered_msg_id`,
		`ALTER TABLE subagent_links ADD COLUMN delivered_msg_id INTEGER`,
		`INSERT INTO subagent_links
			(parent_id, child_id, task_call_id, blocking, depth, state, delivered_at, created_at,
			 result, outcome, activation_seq, delivered_msg_id)
		 VALUES (1, 2, 'call-x', 1, 1, 'completed', 300, 100, 'done', 'completed', 3, 77)`,
	} {
		_, err = db.ExecContext(ctx, stmt)
		require.NoError(t, err, stmt)
	}

	_, err = provider.Up(ctx)
	require.NoError(t, err)

	var (
		createdAt, deliveredAt, deliveredMsgID, activationSeq int64
		result, outcome                                       string
	)
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT created_at, delivered_at, delivered_msg_id, activation_seq, result, outcome
		FROM subagent_links WHERE child_id = 2`).
		Scan(&createdAt, &deliveredAt, &deliveredMsgID, &activationSeq, &result, &outcome))
	assert.Equal(t, int64(100), createdAt)
	assert.Equal(t, int64(300), deliveredAt)
	assert.Equal(t, int64(77), deliveredMsgID)
	assert.Equal(t, int64(3), activationSeq)
	assert.Equal(t, "done", result)
	assert.Equal(t, "completed", outcome)
}
