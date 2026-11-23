package managercontrol

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
)

type inputBackend struct {
	Backend
	store *sessionstore.Store
}

func TestManagerInputAttributesRemainSessionOnly(t *testing.T) {
	t.Parallel()
	_, store, project := newChatControl(t)
	backend := &inputBackend{store: store}
	service := &service{backend: backend, store: store}
	workDir, err := store.GetProjectWorkDir(t.Context(), project)
	require.NoError(t, err)
	id, err := service.createSession(t.Context(), "owner", controllerapi.SessionCreateData{
		WorkDir: workDir, Prompt: "first user message",
		Attributes: map[string]any{"injected": "session-only"},
	})
	require.NoError(t, err)
	err = service.sendSessionMessage(t.Context(), "owner",
		controllerapi.SessionMessageData{SessionID: id, Message: "follow-up"})
	require.NoError(t, err)
	inputs, err := store.ListPending(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, inputs, 2)
	for _, in := range inputs {
		assert.NotContains(t, in.Attributes, "injected")
		assert.Equal(t, "owner", in.Attributes["manager_id"])
	}
}

func (b *inputBackend) CreateSession(
	ctx context.Context,
	project int64,
	_ string,
	prompt, model string,
	attrs map[string]any,
) (int64, error) {
	root, _, err := b.store.CreateManagerRoot(ctx, sessionstore.ManagerRootCreate{
		ProjectID: project, Model: "model", Attributes: attrs, Name: "project", WorkDir: "/project",
		Input: sessionstore.Input{Source: sessionstore.InputSourceUser, Content: prompt},
	})
	if err != nil {
		return 0, err
	}
	return root.ID, nil
}

func (b *inputBackend) SendToSessionResolved(ctx context.Context, id int64, message string) (int64, error) {
	_, err := b.store.Enqueue(
		ctx,
		sessionstore.Input{SessionID: id, Source: sessionstore.InputSourceUser, Content: message},
	)
	return id, err
}

func (*inputBackend) NotifySession(int64, sessionevent.Notification) {}
