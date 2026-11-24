package managercontrol

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionstore/sessionstoretest"
)

func TestChatCapability_SetSessionTitle(t *testing.T) {
	t.Parallel()
	factory, store, project, db := newChatControl(t)
	chat := chatCapability(t, factory, "owner")
	root := sessionstoretest.BareRoot(t, db, project, "model", "", map[string]any{})
	require.NoError(t, chat.SetSessionTitle(t.Context(), "chat", root, " fix login 500 "))
	reloaded, err := store.GetSession(t.Context(), root)
	require.NoError(t, err)
	assert.Equal(t, "fix login 500", reloaded.Attributes[controllerapi.SessionAttributeTitle])
	require.NoError(t, chatCapability(t, factory, "other").SetSessionTitle(t.Context(), "chat", root, "foreign"))
	require.Error(t, chat.SetSessionTitle(t.Context(), "chat", 0, "invalid"))
	require.EqualError(
		t,
		chat.SetSessionTitle(t.Context(), "chat", root, strings.Repeat("界", 81)),
		"title must be one line of 1-80 characters",
	)
	require.ErrorContains(
		t,
		chatCapability(t, factory, "").SetSessionTitle(t.Context(), "chat", root, "x"),
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
