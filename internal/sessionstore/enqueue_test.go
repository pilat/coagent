package sessionstore

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnqueueTx_AssistantCommands(t *testing.T) {
	for _, provenance := range []struct {
		name  string
		attrs map[string]any
	}{
		{"observer", map[string]any{"observer_session_id": nil}},
		{"from", map[string]any{"from": "assistant"}},
	} {
		for _, content := range []string{"/budget 5$", "/config x", "/review", "/stop", "fix the build", "see a/b"} {
			t.Run(provenance.name+"/"+content, func(t *testing.T) {
				t.Parallel()
				store, db, projectID := newTestStore(t)
				root, err := store.CreateSession(
					t.Context(),
					projectID,
					"model",
					"",
					map[string]any{"manager_id": "mgr"},
				)
				require.NoError(t, err)
				err = store.WithTx(t.Context(), func(tx *sql.Tx) error {
					result, enqueueErr := EnqueueTx(t.Context(), tx, Input{
						SessionID: root.ID, Source: InputSourceUser, Content: content, Attributes: provenance.attrs,
					})
					require.NoError(t, enqueueErr)
					require.True(t, result.Applied)
					assert.Equal(t, content, result.Input.RawContent)
					return nil
				})
				require.NoError(t, err)
				var inputs, activations int
				require.NoError(
					t,
					db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM session_inbox WHERE session_id = ?`, root.ID).
						Scan(&inputs),
				)
				require.NoError(
					t,
					db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM session_tool_activations WHERE session_id = ?`, root.ID).
						Scan(&activations),
				)
				assert.Zero(t, activations)
				assert.Equal(t, 1, inputs)
			})
		}
	}
}

func TestEnqueueTx_UserCommand(t *testing.T) {
	t.Parallel()
	store, _, projectID := newTestStore(t)
	root, err := store.CreateSession(t.Context(), projectID, "model", "", map[string]any{"manager_id": "mgr"})
	require.NoError(t, err)
	result, err := store.Enqueue(t.Context(), Input{SessionID: root.ID, Source: InputSourceUser, Content: "/budget 5$"})
	require.NoError(t, err)
	require.True(t, result.Applied)
	assert.Equal(t, "/budget 5$", result.Input.RawContent)
}

func TestActivationRejectsAssistantProvenance(t *testing.T) {
	for _, attrs := range []map[string]any{
		{ObserverSessionIDAttribute: nil},
		{"from": "assistant"},
	} {
		t.Run(fmt.Sprint(attrs), func(t *testing.T) {
			t.Parallel()
			store, db, projectID := newTestStore(t)
			root, err := store.CreateSession(t.Context(), projectID, "model", "", map[string]any{"manager_id": "mgr"})
			require.NoError(t, err)
			result, err := store.Enqueue(t.Context(), Input{
				SessionID: root.ID, Source: InputSourceUser, Content: "/config x", Attributes: attrs,
			})
			require.NoError(t, err)
			_, _, err = acceptActivation(t.Context(), store, result.Input.ID, "/config x",
				ActivationDraft{ToolID: "config_edit", Command: "/config"})
			require.ErrorIs(t, err, ErrActivationConflict)
			assertInputState(t, db, result.Input.ID, InputStatePending)
			var grants int
			require.NoError(t, db.QueryRowContext(t.Context(),
				`SELECT COUNT(*) FROM session_tool_activations WHERE session_id = ?`, root.ID).Scan(&grants))
			assert.Zero(t, grants)
		})
	}
}
