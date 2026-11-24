package sessionstore

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReplacementChain(t *testing.T) {
	store, _, project := newTestStore(t)
	first := chatTestRoot(t, store, project)
	second, _, err := store.ReplaceRoot(t.Context(), first.ID, "test", "/project")
	require.NoError(t, err)
	third, _, err := store.ReplaceRoot(t.Context(), second.ID, "test", "/project")
	require.NoError(t, err)
	for _, id := range []int64{first.ID, second.ID, third.ID} {
		chain, err := store.ReplacementChain(t.Context(), id)
		require.NoError(t, err)
		assert.Equal(t, []int64{first.ID, second.ID, third.ID}, chain)
	}
	killed := chatTestRoot(t, store, project)
	_, err = store.MarkSessionKilledWithOutput(t.Context(), killed.ID, 0)
	require.NoError(t, err)
	chain, err := store.ReplacementChain(t.Context(), killed.ID)
	require.NoError(t, err)
	assert.Equal(t, []int64{killed.ID}, chain)
	_, err = store.ResolveReplacement(t.Context(), killed.ID)
	require.ErrorIs(t, err, ErrSessionNotAcceptingInput)
	foreign, err := store.CreateSubagentSession(t.Context(), project, first.ID, first.ID, "general", "model", "")
	require.NoError(t, err)
	_, err = store.ReplacementChain(t.Context(), foreign)
	require.ErrorIs(t, err, ErrOutputNotRoot)

	t.Run("predecessor query errors are returned", func(t *testing.T) {
		store, db, project := newTestStore(t)
		root := chatTestRoot(t, store, project)
		_, err := db.ExecContext(t.Context(),
			`ALTER TABLE session_outbox RENAME COLUMN attributes TO broken_attributes`)
		require.NoError(t, err)
		_, err = store.ReplacementChain(t.Context(), root.ID)
		require.ErrorContains(t, err, "load session predecessor")
	})

	t.Run("predecessor load errors are returned", func(t *testing.T) {
		store, db, project := newTestStore(t)
		first := chatTestRoot(t, store, project)
		second, _, err := store.ReplaceRoot(t.Context(), first.ID, "test", "/project")
		require.NoError(t, err)
		_, err = db.ExecContext(t.Context(), `UPDATE sessions SET created_at = 'invalid' WHERE id = ?`, first.ID)
		require.NoError(t, err)
		_, err = store.ReplacementChain(t.Context(), second.ID)
		require.ErrorContains(t, err, "load predecessor session")
	})

	t.Run("invalid or missing predecessor stops", func(t *testing.T) {
		store, db, project := newTestStore(t)
		first := chatTestRoot(t, store, project)
		second, _, err := store.ReplaceRoot(t.Context(), first.ID, "test", "/project")
		require.NoError(t, err)
		for _, id := range []any{nil, 0, -1, "invalid", 1.5, 999999} {
			_, err = db.ExecContext(t.Context(), `UPDATE session_outbox
				SET attributes = json_set(attributes, '$.old_session_id', ?)
				WHERE session_id = ? AND type = 'session_replaced'`, id, second.ID)
			require.NoError(t, err)
			chain, err := store.ReplacementChain(t.Context(), second.ID)
			require.NoError(t, err)
			assert.Equal(t, []int64{second.ID}, chain)
		}
	})
}

func TestResolveReplacement_StatusFidelity(t *testing.T) {
	store, db, project := newTestStore(t)
	for _, status := range []SessionStatus{
		SessionStatusActive, SessionStatusCompleted, SessionStatusSuspended, SessionStatusError,
		SessionStatusStopping, SessionStatusStopped, SessionStatusTerminating, SessionStatusKilled,
	} {
		for _, killed := range []bool{false, true} {
			for _, replaced := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/killed=%t/replaced=%t", status, killed, replaced), func(t *testing.T) {
					root := chatTestRoot(t, store, project)
					want := root.ID
					if replaced {
						replacement, _, err := store.ReplaceRoot(t.Context(), root.ID, "test", "/project")
						require.NoError(t, err)
						if status == SessionStatusTerminating || killed {
							want = replacement.ID
						}
					}
					var killedAt *time.Time
					if killed {
						now := time.Now().UTC()
						killedAt = &now
					}
					_, err := db.ExecContext(t.Context(),
						`UPDATE sessions SET status = ?, killed_at = ? WHERE id = ?`, status, killedAt, root.ID)
					require.NoError(t, err)
					resolved, err := store.ResolveReplacement(t.Context(), root.ID)
					if !replaced && (status == SessionStatusTerminating || killed) {
						require.ErrorIs(t, err, ErrSessionNotAcceptingInput)
						assert.Zero(t, resolved)
						return
					}
					require.NoError(t, err)
					assert.Equal(t, want, resolved)
				})
			}
		}
	}
}
