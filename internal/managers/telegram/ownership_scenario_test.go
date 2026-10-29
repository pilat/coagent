package telegram

import (
	"context"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestHarnessScenario_DurableManagerOwnershipReachesOnlyTelegramRenderer(t *testing.T) {
	h := newDelayedTelegramHarness(t)
	project, err := h.sessions.GetOrCreateProject(t.Context(), h.workDir)
	require.NoError(t, err)
	owned, err := h.sessions.CreateSession(t.Context(), project, "fake-model", "", map[string]any{
		"manager_id": delayedTelegramManagerID,
	})
	require.NoError(t, err)
	foreign, err := h.sessions.CreateSession(t.Context(), project, "fake-model", "", map[string]any{
		"manager_id": "telegram-other",
	})
	require.NoError(t, err)

	for _, draft := range []sessionstore.OutputDraft{
		{SessionID: owned.ID, Type: sessionstore.OutputMessage, Content: "owner answer"},
		{SessionID: foreign.ID, Type: sessionstore.OutputMessage, Content: "foreign answer"},
		{SessionID: owned.ID, Type: sessionstore.OutputMessage, Content: "owner barrier"},
	} {
		_, err := h.sessions.EnqueueOutput(t.Context(), draft)
		require.NoError(t, err)
	}

	chatID := strconv.FormatInt(harnessChatID, 10)
	chat := h.controller.(controllerapi.ChatFocusController)
	require.NoError(t, chat.SetChatFocus(t.Context(), chatID, owned.ID))
	foreignChat := h.controllers.ForManager("telegram-other").(controllerapi.ChatFocusController)
	require.Error(t, foreignChat.SetChatFocus(t.Context(), chatID, owned.ID))
	manager := newAssistantTelegramManager(t, h)
	require.NoError(t, manager.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, manager.Stop(context.Background())) })
	awaitChatTail(t, h, owned.ID)

	var messages []string
	for _, call := range h.recorder.snapshot() {
		if call.Method == "sendMessage" {
			messages = append(messages, call.Text)
		}
	}
	assert.Equal(t, []string{"owner answer", "owner barrier"}, messages)
	seen, err := h.sessions.ChatSeen(t.Context(), delayedTelegramManagerID, chatID, foreign.ID)
	require.NoError(t, err)
	assert.Zero(t, seen)
}
