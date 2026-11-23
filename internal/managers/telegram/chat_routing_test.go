package telegram

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pilat/coagent/api/coagent/v1"
	"github.com/pilat/coagent/internal/controllerapi"
)

type chatLoopStub struct {
	focus int64
	wakes int
}

func TestHeartbeatTypingUsesOnlyLoopChatFocus(t *testing.T) {
	var calls []telegramHarnessCall
	manager := newTelegramHarnessManager(t, &fakeController{}, &calls)
	loop := &chatLoopStub{focus: 42}
	manager.delivery = loop
	for _, id := range []int64{42, 9, 99001} {
		manager.handleNotification(t.Context(), &coagentv1.WatchSessionsResponse{
			SessionId: id, Type: coagentv1.SessionEventType_SESSION_EVENT_TYPE_HEARTBEAT,
		})
	}
	loop.focus = 0
	manager.handleNotification(t.Context(), &coagentv1.WatchSessionsResponse{
		SessionId: 0, Type: coagentv1.SessionEventType_SESSION_EVENT_TYPE_HEARTBEAT,
	})
	assert.Equal(t, 4, loop.wakes)
	assert.Equal(t, []telegramHarnessCall{
		{Method: "sendChatAction", ChatID: harnessChatID},
	}, calls)
}

func TestFocusWritesWakeLoopWithoutTheirOwnConfirmation(t *testing.T) {
	var messages []string
	controller := &fakeController{listSessions: []controllerapi.SessionInfo{{
		ID: 42, Attributes: map[string]any{"manager_id": "telegram-main"},
	}}}
	manager := newCmdTestManager(t, controller, &messages)
	loop := &chatLoopStub{}
	manager.delivery = loop
	manager.handleChatText(t.Context(), "/switch 42")
	manager.handleChatText(t.Context(), "/switch 42")
	assert.Equal(t, 2, loop.wakes)
	assert.EqualValues(t, 42, controller.focus)
	assert.Empty(t, messages)
}

func (s *chatLoopStub) Start(context.Context)      {}
func (s *chatLoopStub) Stop(context.Context) error { return nil }
func (s *chatLoopStub) Focus() int64               { return s.focus }
func (s *chatLoopStub) Wake()                      { s.wakes++ }
