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
	for _, observed := range []bool{false, true} {
		t.Run(map[bool]string{false: "without assistant", true: "with assistant"}[observed], func(t *testing.T) {
			t.Parallel()
			_, store, project := newChatControl(t)
			observer := int64(0)
			if observed {
				root, err := store.EnsureAssistantRoot(t.Context(), project, "owner", "/assistant", "model", "")
				require.NoError(t, err)
				observer = root.ID
			}
			backend := &inputBackend{store: store}
			service := &service{backend: backend, store: store}
			workDir, err := store.GetProjectWorkDir(t.Context(), project)
			require.NoError(t, err)
			id, err := service.createSession(t.Context(), "owner", controllerapi.SessionCreateData{
				WorkDir: workDir, Prompt: "first user message",
				Attributes: map[string]any{"from": "assistant", sessionstore.ObserverSessionIDAttribute: 987654},
			})
			require.NoError(t, err)
			_, err = service.sendSessionMessage(
				t.Context(),
				"owner",
				controllerapi.SessionMessageData{SessionID: id, Message: "follow-up"},
			)
			require.NoError(t, err)
			inputs, err := store.ListPending(t.Context(), id)
			require.NoError(t, err)
			require.Len(t, inputs, 2)
			for _, in := range inputs {
				assert.NotContains(t, in.Attributes, "from")
				if observer == 0 {
					assert.NotContains(t, in.Attributes, sessionstore.ObserverSessionIDAttribute)
				} else {
					assert.EqualValues(t, observer, in.Attributes[sessionstore.ObserverSessionIDAttribute])
				}
			}
		})
	}
}

func (b *inputBackend) Send(
	ctx context.Context,
	project int64,
	prompt, model string,
	attrs map[string]any,
) (int64, error) {
	root, _, err := b.store.CreateManagerRoot(ctx, sessionstore.ManagerRootCreate{
		ProjectID: project, Prompt: prompt, Model: "model", Attributes: attrs, Name: "project", WorkDir: "/project",
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
