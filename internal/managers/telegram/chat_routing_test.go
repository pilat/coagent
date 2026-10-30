package telegram

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionevent"
)

type chatLoopStub struct {
	focus int64
	wakes int
}

func TestHeartbeatTypingUsesOnlyLoopChatFocus(t *testing.T) {
	var calls []telegramHarnessCall
	manager := newTelegramHarnessManager(t, &fakeController{}, &calls)
	manager.assistantRootID = 99001
	loop := &chatLoopStub{focus: 42}
	manager.delivery = loop
	for _, id := range []int64{42, 9, 99001} {
		manager.handleNotification(t.Context(), controllerapi.SessionNotification{
			SessionID: id, Notification: sessionevent.Notification{Type: sessionevent.NotifyHeartbeat},
		})
	}
	loop.focus = 0
	manager.handleNotification(t.Context(), controllerapi.SessionNotification{
		SessionID: 99001, Notification: sessionevent.Notification{Type: sessionevent.NotifyHeartbeat},
	})
	assert.Equal(t, 4, loop.wakes)
	assert.Equal(t, []telegramHarnessCall{
		{Method: "sendChatAction", ChatID: harnessChatID}, {Method: "sendChatAction", ChatID: harnessChatID},
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
	manager.handleChatText(t.Context(), "/assistant")
	root, _ := controller.EnsureAssistantRoot(t.Context())
	manager.handleChatText(t.Context(), fmt.Sprintf("/switch %d", root.SessionID))
	assert.Equal(t, 3, loop.wakes)
	assert.Zero(t, controller.focus)
	assert.Empty(t, messages)
}

func TestSessionsListsAssistantUnreadAndCurrentSelection(t *testing.T) {
	var messages []string
	controller := &fakeController{
		focus: 42,
		listSessions: []controllerapi.SessionInfo{
			{ID: 42, Name: "work", Attributes: map[string]any{"manager_id": "telegram-main"}},
			{ID: 9, Name: "other", Attributes: map[string]any{"manager_id": "telegram-main"}},
			{ID: 8, Name: "foreign", Attributes: map[string]any{"manager_id": "another-manager"}},
		},
		outputs: map[int64][]controllerapi.ChatOutput{
			0:  {{ID: 1, Type: controllerapi.OutputMessage, Content: "assistant"}},
			42: {{ID: 2, Type: controllerapi.OutputMessage, Content: "work"}},
		},
	}
	manager := newCmdTestManager(t, controller, &messages)
	manager.handleChatText(t.Context(), "/sessions")
	assert.Equal(t, []string{"#0 Assistant · 1 new\n#42 work · 1 new ◀\n#9 other"}, messages)
}

func (s *chatLoopStub) Start(context.Context)      {}
func (s *chatLoopStub) Stop(context.Context) error { return nil }
func (s *chatLoopStub) Focus() int64               { return s.focus }
func (s *chatLoopStub) Wake()                      { s.wakes++ }
