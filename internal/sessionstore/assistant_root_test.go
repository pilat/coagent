package sessionstore

import (
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
			record, errs[i] = s.EnsureAssistantRoot(ctx, project, "owner", "/project", "model", "medium")
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
}

func TestAssistantRootInitializesMissingModelAndPreservesSelection(t *testing.T) {
	t.Parallel()
	s, db, project := newTestStore(t)
	record, err := s.EnsureAssistantRoot(t.Context(), project, "owner", "/project", "model", "medium")
	require.NoError(t, err)
	assert.Equal(t, "model", record.Model)
	assert.Equal(t, "medium", record.ReasoningLevel)
	again, err := s.EnsureAssistantRoot(t.Context(), project, "owner", "/project", "other-model", "high")
	require.NoError(t, err)
	assert.Equal(t, "model", again.Model)
	assert.Equal(t, "medium", again.ReasoningLevel)
	_, err = db.ExecContext(t.Context(), `UPDATE sessions SET model = '', reasoning_level = '' WHERE id = ?`, record.ID)
	require.NoError(t, err)
	again, err = s.EnsureAssistantRoot(t.Context(), project, "owner", "/project", "other-model", "high")
	require.NoError(t, err)
	assert.Equal(t, record.ID, again.ID)
	stored, err := s.GetSession(t.Context(), record.ID)
	require.NoError(t, err)
	assert.Equal(t, "other-model", stored.Model)
	assert.Equal(t, "high", stored.ReasoningLevel)
}
