package managercontrol

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
)

func TestChatCapability_SetSessionTitle(t *testing.T) {
	t.Parallel()
	factory, store, project := newChatControl(t)
	chat := chatCapability(t, factory, "owner")
	root, err := store.CreateSession(t.Context(), project, "model", "", map[string]any{"manager_id": "owner"})
	require.NoError(t, err)
	require.NoError(t, chat.SetSessionTitle(t.Context(), "chat", root.ID, " fix login 500 "))
	reloaded, err := store.GetSession(t.Context(), root.ID)
	require.NoError(t, err)
	assert.Equal(t, "fix login 500", reloaded.Attributes[controllerapi.SessionAttributeTitle])
	require.Error(t, chatCapability(t, factory, "other").SetSessionTitle(t.Context(), "chat", root.ID, "foreign"))
	require.Error(t, chat.SetSessionTitle(t.Context(), "chat", 0, "assistant"))
	assistant, err := factory.ForManager("owner").EnsureAssistantRoot(t.Context())
	require.NoError(t, err)
	require.Error(t, chat.SetSessionTitle(t.Context(), "chat", assistant.SessionID, "assistant"))
	require.EqualError(
		t,
		chat.SetSessionTitle(t.Context(), "chat", root.ID, strings.Repeat("界", 81)),
		"title must be one line of 1-80 characters",
	)
	require.ErrorContains(
		t,
		chatCapability(t, factory, "").SetSessionTitle(t.Context(), "chat", root.ID, "x"),
		"no manager identity",
	)
}

func TestCreateSession_RejectsCallerSuppliedTitle(t *testing.T) {
	t.Parallel()
	backend := &stubWorktreeBackend{}
	s := worktreeSessionService(t, backend)
	_, err := s.createSession(t.Context(), "owner", controllerapi.SessionCreateData{
		Attributes: map[string]any{controllerapi.SessionAttributeTitle: "injected"},
	})
	require.EqualError(t, err, "title attribute is reserved")
	assert.Nil(t, backend.sentAttrs)
}
