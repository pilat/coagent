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
			ID: 42, Name: "work", Attributes: map[string]any{"manager_id": "telegram-main"},
		}}}
		manager := newCmdTestManager(t, controller, &messages)
		hook := &focusWriteHook{ChatFocusController: manager.chat}
		manager.chat = hook
		loop := managerdelivery.NewChat(&chatPort{controller: hook, chatID: "chat"}, &outputTransport{manager: manager})
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
			assert.Equal(t, []string{"→ #42 work"}, messages)
		}
		manager.handleChatText(t.Context(), "/switch 42")
		synctest.Wait()
		assert.Equal(t, []string{"→ #42 work"}, messages)
	})
}

func TestFocusWriteFailureClearsPendingEntry(t *testing.T) {
	t.Parallel()
	var messages []string
	manager := newCmdTestManager(t, &fakeController{}, &messages)
	loop := &chatLoopStub{target: -1}
	manager.delivery = loop
	manager.chat = &focusWriteHook{
		ChatFocusController: manager.chat,
		beforeWrite:         func() { assert.EqualValues(t, 42, loop.target) },
	}
	manager.handleChatText(t.Context(), "/switch 42")
	assert.Equal(t, []int64{42, -1}, loop.enterTargets)
	assert.EqualValues(t, -1, loop.target)
	assert.Zero(t, loop.wakes)
	assert.Equal(t, []string{"#42 is not a live session"}, messages)
}

func (h *focusWriteHook) SetChatFocus(ctx context.Context, chatID string, id int64) error {
	h.beforeWrite()
	if err := h.ChatFocusController.SetChatFocus(ctx, chatID, id); err != nil {
		return err
	}
	h.afterWrite()
	return nil
}
