package telegram

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/managerdelivery"
)

type focusWriteHook struct {
	controllerapi.ChatFocusController
	beforeWrite func()
	afterWrite  func()
}

func TestSwitchObservedBeforeSetReturnsSendsOnePrelude(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var messages []string
		controller := &fakeController{listSessions: []controllerapi.SessionInfo{{
			ID: 42, Name: "work", Attributes: map[string]any{},
		}}}
		manager := newCmdTestManager(t, controller, &messages)
		hook := &focusWriteHook{ChatFocusController: controller}
		manager.api = servedAPI(t, controller, hook)
		loop := managerdelivery.NewChat(&chatPort{api: manager.api, chatID: "chat"}, &outputTransport{manager: manager})
		manager.delivery = loop
		loop.Start(t.Context())
		defer func() { require.NoError(t, loop.Stop(t.Context())) }()
		synctest.Wait()
		hook.beforeWrite = func() {
			synctest.Wait()
			assert.Empty(t, messages)
		}
		hook.afterWrite = func() {
			loop.Wake()
			synctest.Wait()
			assert.Equal(t, []string{"<b>→ #42 work</b>"}, messages)
		}
		manager.handleChatText(t.Context(), "/switch 42")
		synctest.Wait()
		assert.Equal(t, []string{"<b>→ #42 work</b>"}, messages)
	})
}

func TestFocusWriteFailureDoesNotWakeDelivery(t *testing.T) {
	t.Parallel()
	var messages []string
	fake := &fakeController{}
	manager := newCmdTestManager(t, fake, &messages)
	loop := &chatLoopStub{}
	manager.delivery = loop
	manager.api = servedAPI(t, fake, &focusWriteHook{
		ChatFocusController: fake,
		beforeWrite:         func() { assert.Zero(t, loop.wakes) },
	})
	manager.handleChatText(t.Context(), "/switch 42")
	assert.Zero(t, loop.wakes)
	assert.Equal(t, []string{"#42 is not a live session"}, messages)
}

func (h *focusWriteHook) SetChatFocus(ctx context.Context, chatID string, id int64) error {
	if h.beforeWrite != nil {
		h.beforeWrite()
	}
	if err := h.ChatFocusController.SetChatFocus(ctx, chatID, id); err != nil {
		return err
	}
	if h.afterWrite != nil {
		h.afterWrite()
	}
	return nil
}
