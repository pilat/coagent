package managercontrol

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
)

func TestCreateSession_RejectsCallerSuppliedOrchestrator(t *testing.T) {
	backend := &stubWorktreeBackend{}
	s := worktreeSessionService(t, backend)
	_, err := s.createSession(t.Context(), "owner", controllerapi.SessionCreateData{
		Attributes: map[string]any{"orchestrator": true},
	})
	require.EqualError(t, err, "orchestrator attribute is reserved")
	assert.Nil(t, backend.sentAttrs)
}

func TestCreateSession_RejectsCallerSuppliedSpawnedBy(t *testing.T) {
	for _, value := range []any{int64(12), float64(12), "12", nil} {
		backend := &stubWorktreeBackend{}
		s := worktreeSessionService(t, backend)
		_, err := s.createSession(t.Context(), "owner", controllerapi.SessionCreateData{
			Attributes: map[string]any{"spawned_by": value},
		})
		require.EqualError(t, err, "spawned_by attribute is reserved")
		assert.Nil(t, backend.sentAttrs)
	}
}

func TestOrchestratorCapability(t *testing.T) {
	factory, store, project := newChatControl(t)
	root, err := store.CreateSession(t.Context(), project, "model", "", map[string]any{"manager_id": "owner"})
	require.NoError(t, err)
	for _, owner := range []string{"owner", "other", ""} {
		capability, ok := factory.ForManager(owner).(controllerapi.ChatFocusController)
		require.True(t, ok)
		if owner == "owner" {
			changed, err := capability.SetOrchestrator(t.Context(), "chat", root.ID, true)
			require.NoError(t, err)
			assert.True(t, changed)
			on, err := capability.Orchestrator(t.Context(), "chat", root.ID)
			require.NoError(t, err)
			assert.True(t, on)
			_, err = capability.SetOrchestrator(t.Context(), "chat", 0, true)
			require.EqualError(t, err, "#0 is not a live session")
		} else {
			_, err := capability.SetOrchestrator(t.Context(), "chat", root.ID, true)
			if owner == "" {
				require.ErrorContains(t, err, "no manager identity")
			} else {
				require.EqualError(t, err, fmt.Sprintf("#%d is not a live session", root.ID))
			}
			_, err = capability.Orchestrator(t.Context(), "chat", root.ID)
			require.Error(t, err)
		}
	}
}
