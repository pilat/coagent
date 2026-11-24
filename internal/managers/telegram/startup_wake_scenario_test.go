package telegram

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestHarnessScenario_OutputRightAfterStartIsDeliveredByItsWake(t *testing.T) {
	h := newDelayedTelegramHarness(t)
	id := createChatWork(t, h)
	chat := h.controller.(controllerapi.ChatFocusController)
	key := strconv.FormatInt(harnessChatID, 10)
	require.NoError(t, chat.SetChatFocus(t.Context(), key, id))
	_, newest, err := chat.ChatEntry(t.Context(), key, id, 3)
	require.NoError(t, err)
	entered, err := chat.EnterChat(t.Context(), key, id, newest)
	require.NoError(t, err)
	require.True(t, entered)
	manager := newChatTelegramManager(t, h)
	require.NoError(t, manager.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, manager.Stop(context.Background())) })
	_, err = h.sessions.EnqueueOutput(t.Context(), sessionstore.OutputDraft{
		SessionID: id, Type: sessionstore.OutputMessage, Content: "x",
	})
	require.NoError(t, err)
	h.bus.Publish(
		id,
		true,
		sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "x"},
	)
	require.Eventually(t, func() bool {
		return hasTelegramMessage(h.recorder.snapshot(), fmt.Sprintf("<b>#%d project</b>\n\nx", id))
	}, time.Second, time.Millisecond)
}
