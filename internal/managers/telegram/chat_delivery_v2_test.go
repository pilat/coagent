package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
)

func TestSessionsLimitAndNumberedControlBeyondLimit(t *testing.T) {
	for _, total := range []int{29, 30, 35} {
		t.Run(strconv.Itoa(total), func(t *testing.T) {
			t.Parallel()
			ctrl := &fakeController{}
			for id := 1; id < total; id++ {
				ctrl.listSessions = append(ctrl.listSessions, controllerapi.SessionInfo{
					ID: int64(id), Name: fmt.Sprintf("work %d", id),
					Attributes: map[string]any{"manager_id": "telegram-main"},
				})
			}
			var messages []string
			manager := newCmdTestManager(t, ctrl, &messages)
			manager.handleChatText(t.Context(), "/sessions")
			require.Len(t, messages, 1)
			lines := strings.Split(messages[0], "\n")
			assert.Equal(t, "#0 Assistant ◀", lines[0])
			if total <= 30 {
				assert.Len(t, lines, total)
				assert.NotContains(t, messages[0], "more —")
				return
			}
			require.Len(t, lines, 31)
			assert.Equal(t, "#29 work 29", lines[29])
			assert.Equal(t, "… and 5 more — ask the assistant to find one", lines[30])
			manager.handleChatText(t.Context(), "/kill 34")
			assert.Equal(t, []controllerapi.SessionMessageData{{SessionID: 34, Message: "/kill"}}, ctrl.messageCalls)
			assert.Zero(t, ctrl.focus)
		})
	}
}

func TestHarnessScenario_DigestOnAssistantReturnAndSeenContextOnReentry(t *testing.T) {
	h := newDelayedTelegramHarness(t)
	one, two := createChatWork(t, h), createChatWork(t, h)
	manager := newAssistantTelegramManager(t, h)
	require.NoError(t, manager.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, manager.Stop(context.Background())) })
	awaitChatTail(t, h, manager.assistantRootID)
	enqueueChatMessage(t, h, one, "other unread")
	output := enqueueChatMessage(t, h, two, "work answer")
	manager.processUpdate(t.Context(), assistantTelegramUpdate(fmt.Sprintf("/switch %d", two)))
	awaitChatTail(t, h, two)
	messages := func() []string {
		var texts []string
		for _, call := range h.recorder.snapshot() {
			if call.Method == "sendMessage" {
				texts = append(texts, call.Text)
			}
		}
		return texts
	}
	want := []string{fmt.Sprintf("→ #%d project", two), fmt.Sprintf("#%d project\nwork answer", two)}
	assert.Equal(t, want, messages())
	manager.processUpdate(t.Context(), assistantTelegramUpdate("/assistant"))
	digest := fmt.Sprintf("#%d project · 1 new · other unread", one)
	require.Eventually(t, func() bool { return hasTelegramMessage(h.recorder.snapshot(), digest) },
		time.Second, time.Millisecond)
	want = append(want, "→ #0 Assistant", digest)
	assert.Equal(t, want, messages())
	manager.processUpdate(t.Context(), assistantTelegramUpdate("/assistant"))
	want = append(want, "→ #0 Assistant", digest)
	require.Eventually(t, func() bool { return len(messages()) == len(want) }, time.Second, time.Millisecond)
	assert.Equal(t, want, messages())
	manager.processUpdate(t.Context(), assistantTelegramUpdate(fmt.Sprintf("/switch %d", manager.assistantRootID)))
	want = append(want, "→ #0 Assistant", digest)
	require.Eventually(t, func() bool { return len(messages()) == len(want) }, time.Second, time.Millisecond)
	assert.Equal(t, want, messages())
	manager.processUpdate(t.Context(), assistantTelegramUpdate(fmt.Sprintf("/switch %d", two)))
	want = append(want, fmt.Sprintf("→ #%d project", two), fmt.Sprintf("#%d project\nwork answer", two))
	require.Eventually(t, func() bool { return len(messages()) == len(want) }, time.Second, time.Millisecond)
	assert.Equal(t, want, messages())
	seen, err := h.sessions.ChatSeen(
		t.Context(),
		delayedTelegramManagerID,
		strconv.FormatInt(manager.effectiveChatID(), 10),
		two,
	)
	require.NoError(t, err)
	assert.Equal(t, output, seen)
	seen, err = h.sessions.ChatSeen(
		t.Context(),
		delayedTelegramManagerID,
		strconv.FormatInt(manager.effectiveChatID(), 10),
		one,
	)
	require.NoError(t, err)
	assert.Zero(t, seen)
}
