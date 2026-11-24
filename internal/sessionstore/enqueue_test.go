package sessionstore

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/sessionstore/sessionstoretest"
)

func TestEnqueueTx_UserCommand(t *testing.T) {
	t.Parallel()
	store, _, projectID := newTestStore(t)
	root := sessionstoretest.BareRoot(t, store.db, projectID, "model", "", map[string]any{})
	result, err := store.Enqueue(t.Context(), Input{SessionID: root, Source: InputSourceUser, Content: "/budget 5$"})
	require.NoError(t, err)
	require.True(t, result.Applied)
	assert.Equal(t, "/budget 5$", result.Input.RawContent)
}

func TestEnqueueAgentInputActivatesStoppedRoot(t *testing.T) {
	for _, kind := range []string{"stopped root", "fired budget", "subagent"} {
		t.Run(kind, func(t *testing.T) {
			store, db, project := newTestStore(t)
			root := chatTestRoot(t, store, project)
			id := root.ID
			if kind == "subagent" {
				var err error
				id, err = store.CreateSubagentSession(t.Context(), project, root.ID, root.ID, "general", "model", "")
				require.NoError(t, err)
			}
			if kind == "fired budget" {
				armed, err := store.Arm(t.Context(), targetedBudgetMutation(t, store, id, id, "arm"))
				require.NoError(t, err)
				_, _, err = store.FireBudget(t.Context(), id, armed.Generation, "duration", 0, "checkpoint")
				require.NoError(t, err)
			}
			old := time.Now().UTC().Add(-time.Hour)
			_, err := db.ExecContext(
				t.Context(),
				"UPDATE sessions SET status = 'stopped', episode_started_at = ? WHERE id = ?",
				old,
				id,
			)
			require.NoError(t, err)
			_, err = store.Enqueue(t.Context(), Input{
				SessionID: id, Source: InputSourceAgent, Content: "hello",
				Attributes: map[string]any{"sender_session_id": 12, "sender_name": "coagent"},
			})
			require.NoError(t, err)
			record, err := store.GetSession(t.Context(), id)
			require.NoError(t, err)
			var episode time.Time
			require.NoError(
				t,
				db.QueryRowContext(t.Context(), "SELECT episode_started_at FROM sessions WHERE id = ?", id).
					Scan(&episode),
			)
			if kind == "stopped root" {
				assert.Equal(t, SessionStatusActive, record.Status)
				assert.True(t, episode.After(old))
			} else {
				assert.Equal(t, SessionStatusStopped, record.Status)
				assert.WithinDuration(t, old, episode, time.Millisecond)
			}
			if kind == "fired budget" {
				current, err := store.Get(t.Context(), id)
				require.NoError(t, err)
				assert.Equal(t, budget.Fired, current.State)
			}
		})
	}
}
