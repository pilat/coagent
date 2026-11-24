package telegram

import (
	"context"
	"fmt"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/sessionstore/sessionstoretest"
)

func TestHarnessScenario_TelegramRendersOnlyItsFocusWhileEveryRootWakesIt(t *testing.T) {
	h := newDelayedTelegramHarness(t)
	project, err := h.sessions.GetOrCreateProject(t.Context(), h.workDir)
	require.NoError(t, err)
	owned := sessionstoretest.BareRoot(t, h.db, project, "fake-model", "", map[string]any{})
	foreignID := sessionstoretest.BareRoot(t, h.db, project, "fake-model", "", map[string]any{})
	foreign, err := h.sessions.GetSession(t.Context(), foreignID)
	require.NoError(t, err)

	for _, draft := range []sessionstore.OutputDraft{
		{SessionID: owned, Type: sessionstore.OutputMessage, Content: "owner answer"},
		{SessionID: foreign.ID, Type: sessionstore.OutputMessage, Content: "foreign answer"},
		{SessionID: owned, Type: sessionstore.OutputMessage, Content: "owner barrier"},
	} {
		_, err := h.sessions.EnqueueOutput(t.Context(), draft)
		require.NoError(t, err)
	}

	chatID := strconv.FormatInt(harnessChatID, 10)
	chat := h.controller.(controllerapi.ChatFocusController)
	require.NoError(t, chat.SetChatFocus(t.Context(), chatID, owned))
	foreignChat := h.controllers.ForManager("telegram-other").(controllerapi.ChatFocusController)
	require.NoError(t, foreignChat.SetChatFocus(t.Context(), chatID, owned))
	manager := newChatTelegramManager(t, h)
	require.NoError(t, manager.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, manager.Stop(context.Background())) })
	awaitChatTail(t, h, owned)

	for _, draft := range []sessionstore.OutputDraft{
		{SessionID: foreign.ID, Type: sessionstore.OutputMessage, Content: "foreign later"},
		{SessionID: owned, Type: sessionstore.OutputMessage, Content: "owner later"},
	} {
		_, err := h.sessions.EnqueueOutput(t.Context(), draft)
		require.NoError(t, err)
		h.bus.Publish(
			draft.SessionID,
			true,
			sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: draft.Content},
		)
	}
	awaitChatTail(t, h, owned)

	var messages []string
	for _, call := range h.recorder.snapshot() {
		if call.Method == "sendMessage" {
			messages = append(messages, call.Text)
		}
	}
	assert.Equal(t, []string{
		fmt.Sprintf("<b>→ #%d project</b>", owned),
		fmt.Sprintf("<b>#%d project</b>\n\nowner answer", owned),
		fmt.Sprintf("<b>#%d project</b>\n\nowner barrier", owned),
		fmt.Sprintf("<b>#%d project</b>\n\nowner later", owned),
	}, messages)
	for _, text := range messages {
		assert.NotContains(t, text, fmt.Sprintf("#%d project", foreign.ID))
	}
}
