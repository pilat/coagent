package sessionstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
