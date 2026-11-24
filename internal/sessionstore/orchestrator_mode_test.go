package sessionstore

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
)

func TestSetOrchestrator(t *testing.T) {
	store, db, project := newTestStore(t)
	root := chatTestRoot(t, store, project)
	for i, on := range []bool{true, true, false, false} {
		changed, err := store.SetOrchestrator(t.Context(), root.ID, on)
		require.NoError(t, err)
		assert.Equal(t, i%2 == 0, changed)
		assertOrchestratorMode(t, store, root.ID, on)
	}
	for _, invalid := range []any{1, "true", false, nil} {
		raw, err := json.Marshal(map[string]any{"orchestrator": invalid})
		require.NoError(t, err)
		_, err = db.ExecContext(t.Context(), "UPDATE sessions SET attributes = ? WHERE id = ?", string(raw), root.ID)
		require.NoError(t, err)
		assertOrchestratorMode(t, store, root.ID, false)
		changed, err := store.SetOrchestrator(t.Context(), root.ID, true)
		require.NoError(t, err)
		assert.True(t, changed)
		assertOrchestratorMode(t, store, root.ID, true)
	}
	killed := chatTestRoot(t, store, project)
	_, err := store.MarkSessionKilledWithOutput(t.Context(), killed.ID, 0)
	require.NoError(t, err)
	child, err := store.CreateSubagentSession(t.Context(), project, root.ID, root.ID, "general", "model", "")
	require.NoError(t, err)
	_, err = db.ExecContext(
		t.Context(),
		`UPDATE sessions SET attributes = '{}' WHERE id = ?`,
		child,
	)
	require.NoError(t, err)
	for _, id := range []int64{killed.ID, child, 999} {
		for _, on := range []bool{true, false} {
			_, err := store.SetOrchestrator(t.Context(), id, on)
			require.EqualError(t, err, fmt.Sprintf("#%d is not a live session", id))
		}
		_, err := store.OrchestratorMode(t.Context(), id)
		require.EqualError(t, err, fmt.Sprintf("#%d is not a live session", id))
	}
}

func TestReplaceManagerRootKeepsOrchestratorMode(t *testing.T) {
	store, _, project := newTestStore(t)
	root := chatTestRoot(t, store, project)
	require.NoError(t, store.SetSessionTitle(t.Context(), root.ID, "old title"))
	_, err := store.SetOrchestrator(t.Context(), root.ID, true)
	require.NoError(t, err)
	replacement, _, err := store.ReplaceManagerRoot(t.Context(), root.ID, "test", "/project")
	require.NoError(t, err)
	assert.True(t, controllerapi.OrchestratorMode(replacement.Attributes))
	assert.NotContains(t, replacement.Attributes, TitleAttribute)
}

func assertOrchestratorMode(t *testing.T, store *Store, id int64, want bool) {
	t.Helper()
	mode, err := store.OrchestratorMode(t.Context(), id)
	require.NoError(t, err)
	record, err := store.GetSession(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, want, mode)
	assert.Equal(t, want, controllerapi.OrchestratorMode(record.Attributes))
}
