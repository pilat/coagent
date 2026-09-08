package migrate

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrate_35_RemovesOnlyUnresolvedProcessEventOutboxLeaks(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "process-event-outbox.db")
	db, err := OpenDB(ctx, dbPath)
	require.NoError(t, err)
	defer db.Close()

	provider := newProvider(t, db)
	_, err = provider.UpTo(ctx, 34)
	require.NoError(t, err)
	seedBackgroundProcessSessions(ctx, t, db)

	_, err = db.ExecContext(ctx, `INSERT INTO session_outbox
		(session_id, type, content, attributes, source_key, fingerprint, created_at)
		VALUES
		(1, 'message_persistent', 'leaked', '{"source":"scheduler"}',
		 'schedule:bgp_1:announcement', 'leaked-fingerprint', datetime('now')),
		(1, 'message_persistent', 'scheduled', '{"source":"scheduler"}',
		 'schedule:schedule:one-shot:1:announcement', 'scheduled-fingerprint', datetime('now')),
		(1, 'message_persistent', 'other', '{"source":"agent"}',
		 'schedule:bgp_2:announcement', 'other-fingerprint', datetime('now'))`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `INSERT INTO session_outbox
		(session_id, type, content, attributes, source_key, fingerprint, state,
		 attempt_seq, last_attempt_at, delivered_at, created_at)
		VALUES (1, 'message_persistent', 'historical leak', '{"source":"scheduler"}',
		 'schedule:bgp_delivered:announcement', 'delivered-fingerprint', 'delivered',
		 1, datetime('now'), datetime('now'), datetime('now'))`)
	require.NoError(t, err)

	_, err = provider.Up(ctx)
	require.NoError(t, err)

	rows, err := db.QueryContext(ctx, `SELECT content FROM session_outbox ORDER BY id`)
	require.NoError(t, err)
	defer rows.Close()

	var contents []string
	for rows.Next() {
		var content string
		require.NoError(t, rows.Scan(&content))
		contents = append(contents, content)
	}
	require.NoError(t, rows.Err())

	assert.Equal(t, []string{"scheduled", "other", "historical leak"}, contents)
}
