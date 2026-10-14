package sessionstore

import (
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAssistantRootConcurrentEnsureAndProgress(t *testing.T) {
	t.Parallel()
	s, db, project := newTestStore(t)
	ctx := t.Context()
	var wg sync.WaitGroup
	ids := make([]int64, 4)
	errs := make([]error, 4)
	for i := range ids {
		wg.Go(func() {
			var record *SessionRecord
			record, errs[i] = s.EnsureAssistantRoot(ctx, project, "owner", "/project")
			if record != nil {
				ids[i] = record.ID
			}
		})
	}
	wg.Wait()
	for i, id := range ids {
		require.NoError(t, errs[i])
		assert.Equal(t, ids[0], id)
	}
	var opened int
	require.NoError(
		t,
		db.QueryRowContext(ctx, `SELECT count(*) FROM session_outbox WHERE type = 'session_opened'`).Scan(&opened),
	)
	assert.Equal(t, 1, opened)
	_, err := db.ExecContext(
		ctx,
		`UPDATE sessions SET status = 'active', episode_started_at = ? WHERE id = ?`,
		time.Now().UTC(),
		ids[0],
	)
	require.NoError(t, err)
	roots, err := s.ListAutonomousProgressRoots(ctx)
	require.NoError(t, err)
	assert.NotContains(t, roots, ids[0])
	require.NoError(
		t,
		s.WithTx(ctx, func(tx *sql.Tx) error { return PublishAssistantMessageTx(ctx, tx, ids[0], "plain reply") }),
	)
	var content string
	require.NoError(
		t,
		db.QueryRowContext(ctx, `SELECT content FROM session_outbox WHERE type = 'message_persistent'`).Scan(&content),
	)
	assert.Equal(t, "plain reply", content)
}
