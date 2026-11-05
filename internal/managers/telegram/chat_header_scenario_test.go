package telegram

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
)

func TestHarnessScenario_ChatHeadersStripStarsOnlyFromDisplay(t *testing.T) {
	h := newDelayedTelegramHarness(t)
	id := createChatWork(t, h)
	title := "*daily* **check** <next>"
	require.NoError(t, h.sessions.SetSessionTitle(t.Context(), delayedTelegramManagerID, id, title))
	enqueueChatMessage(t, h, id, "**entry body**")
	manager := newChatTelegramManager(t, h)
	require.NoError(t, manager.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, manager.Stop(context.Background())) })
	manager.processUpdate(t.Context(), chatTelegramUpdate(fmt.Sprintf("/switch %d", id)))
	awaitChatTail(t, h, id)
	enqueueChatMessage(t, h, id, "*live body*")
	manager.wakeDelivery()
	awaitChatTail(t, h, id)
	assert.Equal(t, []string{
		fmt.Sprintf("<b>→ #%d project · daily check &lt;next&gt;</b>", id),
		fmt.Sprintf("<b>#%d project · daily check &lt;next&gt;</b>\n\n<b>entry body</b>", id),
		fmt.Sprintf("<b>#%d project · daily check &lt;next&gt;</b>\n\n<i>live body</i>", id),
	}, chatMessages(h))
	chat := h.controller.(controllerapi.ChatFocusController)
	sessions, err := chat.ChatSessions(t.Context(), fmt.Sprint(harnessChatID))
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, "project · "+title, sessions[0].Name)
	rows, err := chat.ChatOutputs(t.Context(), fmt.Sprint(harnessChatID), id, 0, 10)
	require.NoError(t, err)
	var contents []string
	for _, row := range rows {
		if row.Type == controllerapi.OutputMessage {
			contents = append(contents, row.Content)
		}
	}
	assert.Equal(t, []string{"**entry body**", "*live body*"}, contents)
}
