package managercontrol

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionstore/sessionstoretest"
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
	factory, _, project, db := newChatControl(t)
	root := sessionstoretest.BareRoot(t, db, project, "model", "", map[string]any{})
	for _, owner := range []string{"owner", "other", ""} {
		capability, ok := factory.ForManager(owner).(controllerapi.ChatFocusController)
		require.True(t, ok)
		switch owner {
		case "owner":
			changed, err := capability.SetOrchestrator(t.Context(), "chat", root, true)
			require.NoError(t, err)
			assert.True(t, changed)
			on, err := capability.Orchestrator(t.Context(), "chat", root)
			require.NoError(t, err)
			assert.True(t, on)
			_, err = capability.SetOrchestrator(t.Context(), "chat", 0, true)
			require.EqualError(t, err, "#0 is not a live session")
		case "other":
			changed, err := capability.SetOrchestrator(t.Context(), "chat", root, true)
			require.NoError(t, err)
			assert.False(t, changed)
			on, err := capability.Orchestrator(t.Context(), "chat", root)
			require.NoError(t, err)
			assert.True(t, on)
		default:
			_, err := capability.SetOrchestrator(t.Context(), "chat", root, true)
			require.ErrorContains(t, err, "no manager identity")
			_, err = capability.Orchestrator(t.Context(), "chat", root)
			require.Error(t, err)
		}
	}
}
