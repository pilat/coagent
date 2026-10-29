package telegram

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestHarnessScenario_SwitchDeliversHeaderDigestAndTail(t *testing.T) {
	h := newDelayedTelegramHarness(t)
	one := createChatWork(t, h)
	two := createChatWork(t, h)
	three := createChatWork(t, h)
	enqueueChatMessage(t, h, one, "startup")
	chat := h.controller.(controllerapi.ChatFocusController)
	require.NoError(t, chat.SetChatFocus(t.Context(), strconv.FormatInt(harnessChatID, 10), one))
	manager := newAssistantTelegramManager(t, h)
	require.NoError(t, manager.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, manager.Stop(context.Background())) })
	awaitChatTail(t, h, one)

	enqueueChatMessage(t, h, three, "third unread")
	enqueueChatMessage(t, h, two, "new tail one")
	enqueueChatMessage(t, h, two, "new tail two")
	manager.processUpdate(t.Context(), assistantTelegramUpdate(fmt.Sprintf("/switch %d", two)))
	awaitChatTail(t, h, two)
	var messages []string
	for _, call := range h.recorder.snapshot() {
		if call.Method == "sendMessage" {
			messages = append(messages, call.Text)
		}
	}
	assert.Equal(t, []string{
		"startup", fmt.Sprintf("→ #%d project - %d", two, two),
		fmt.Sprintf("#%d project - %d · 1 new · third unread", three, three),
		"new tail one", "new tail two",
	}, messages)
	seen, err := h.sessions.ChatSeen(t.Context(), delayedTelegramManagerID, strconv.FormatInt(harnessChatID, 10), three)
	require.NoError(t, err)
	assert.Zero(t, seen)
}

func TestHarnessScenario_NumberedControlsAndTextKeepTheirRouting(t *testing.T) {
	h := newDelayedTelegramHarness(t)
	one := createChatWork(t, h)
	two := createChatWork(t, h)
	chat := h.controller.(controllerapi.ChatFocusController)
	key := strconv.FormatInt(harnessChatID, 10)
	require.NoError(t, chat.SetChatFocus(t.Context(), key, one))
	manager := newAssistantTelegramManager(t, h)
	manager.processUpdate(t.Context(), assistantTelegramUpdate(fmt.Sprintf("/stop %d", two)))
	manager.processUpdate(t.Context(), assistantTelegramUpdate(fmt.Sprintf("/kill %d", two)))
	manager.processUpdate(t.Context(), assistantTelegramUpdate("plain focused text"))
	focus, err := chat.ChatFocus(t.Context(), key)
	require.NoError(t, err)
	assert.Equal(t, one, focus)
	var inputs []string
	require.NoError(t, h.sessions.WithTx(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(
			t.Context(),
			`SELECT raw_content FROM session_inbox WHERE session_id = ? ORDER BY id`,
			two,
		)
		if err != nil {
			return fmt.Errorf("read control inputs: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var text string
			if err := rows.Scan(&text); err != nil {
				return fmt.Errorf("scan control input: %w", err)
			}
			inputs = append(inputs, text)
		}
		return rows.Err()
	}))
	assert.Equal(t, []string{"/stop", "/kill"}, inputs)
	var count int
	require.NoError(t, h.sessions.WithTx(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM session_inbox
			WHERE session_id = ? AND raw_content = 'plain focused text'`, one).Scan(&count)
	}))
	assert.Equal(t, 1, count)
}

func TestHarnessScenario_KilledSwitchRefusedAndFocusedKillDrainsClose(t *testing.T) {
	h := newDelayedTelegramHarness(t)
	id := createChatWork(t, h)
	chat := h.controller.(controllerapi.ChatFocusController)
	key := strconv.FormatInt(harnessChatID, 10)
	require.NoError(t, chat.SetChatFocus(t.Context(), key, id))
	manager := newAssistantTelegramManager(t, h)
	require.NoError(t, manager.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, manager.Stop(context.Background())) })
	awaitChatTail(t, h, id)
	_, err := h.sessions.MarkSessionKilledWithOutput(t.Context(), id, 3)
	require.NoError(t, err)
	manager.wakeDelivery()
	require.Eventually(t, func() bool {
		return hasTelegramMessage(h.recorder.snapshot(), "→ #0 Assistant")
	}, 3*time.Second, 10*time.Millisecond)
	manager.processUpdate(t.Context(), assistantTelegramUpdate(fmt.Sprintf("/switch %d", id)))
	var messages []string
	for _, call := range h.recorder.snapshot() {
		if call.Method == "sendMessage" {
			messages = append(messages, call.Text)
		}
	}
	assert.Equal(t, []string{
		"Session killed. Cancelled background processes: 3", "→ #0 Assistant",
		fmt.Sprintf("#%d is not a live session", id),
	}, messages)
	focus, err := chat.ChatFocus(t.Context(), key)
	require.NoError(t, err)
	assert.Zero(t, focus)
}

func TestHarnessScenario_ForbiddenChatPausesWithoutMarking(t *testing.T) {
	h := newDelayedTelegramHarness(t)
	id := createChatWork(t, h)
	outputID := enqueueChatMessage(t, h, id, "must remain unread")
	chat := h.controller.(controllerapi.ChatFocusController)
	key := strconv.FormatInt(harnessChatID, 10)
	require.NoError(t, chat.SetChatFocus(t.Context(), key, id))
	h.recorder.sendResponses = []string{`{"ok":false,"error_code":403,"description":"bot blocked"}`}
	manager := newAssistantTelegramManager(t, h)
	require.NoError(t, manager.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, manager.Stop(context.Background())) })
	require.Eventually(t, func() bool {
		return hasTelegramMessage(h.recorder.snapshot(), "must remain unread")
	}, time.Second, time.Millisecond)
	require.NoError(t, manager.Stop(t.Context()))
	seen, err := h.sessions.ChatSeen(t.Context(), delayedTelegramManagerID, key, id)
	require.NoError(t, err)
	assert.Less(t, seen, outputID)
	rows, err := chat.ChatOutputs(t.Context(), key, id, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "must remain unread", rows[0].Content)
}

func TestHarnessScenario_ParseRejectionFallsBackBeforeMarking(t *testing.T) {
	h := newDelayedTelegramHarness(t)
	id := createChatWork(t, h)
	outputID := enqueueChatMessage(t, h, id, "**answer**")
	chat := h.controller.(controllerapi.ChatFocusController)
	key := strconv.FormatInt(harnessChatID, 10)
	require.NoError(t, chat.SetChatFocus(t.Context(), key, id))
	h.recorder.sendResponses = []string{`{"ok":false,"error_code":400,"description":"can't parse entities"}`}
	manager := newAssistantTelegramManager(t, h)
	require.NoError(t, manager.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, manager.Stop(context.Background())) })
	awaitChatTail(t, h, id)
	var sends []delayedTelegramCall
	for _, call := range h.recorder.snapshot() {
		if call.Method == "sendMessage" {
			sends = append(sends, call)
		}
	}
	assert.Equal(t, []delayedTelegramCall{
		{Method: "sendMessage", Text: "<b>answer</b>", ChatID: harnessChatID, ParseMode: tgParseModeHTML},
		{Method: "sendMessage", Text: "answer", ChatID: harnessChatID},
	}, sends)
	seen, err := h.sessions.ChatSeen(t.Context(), delayedTelegramManagerID, key, id)
	require.NoError(t, err)
	assert.Equal(t, outputID, seen)
}

func createChatWork(t *testing.T, h *delayedTelegramHarness) int64 {
	t.Helper()
	project, err := h.sessions.GetOrCreateProject(t.Context(), h.workDir)
	require.NoError(t, err)
	root, _, err := h.sessions.CreateManagerRoot(t.Context(), sessionstore.ManagerRootCreate{
		ProjectID: project, Model: "fake-model", Name: "project", WorkDir: h.workDir,
		Attributes: map[string]any{"manager_id": delayedTelegramManagerID},
	})
	require.NoError(t, err)
	return root.ID
}

func enqueueChatMessage(t *testing.T, h *delayedTelegramHarness, sessionID int64, content string) int64 {
	t.Helper()
	commit, err := h.sessions.EnqueueOutput(t.Context(), sessionstore.OutputDraft{
		SessionID: sessionID, Type: sessionstore.OutputMessage, Content: content,
	})
	require.NoError(t, err)
	return commit.OutputID
}
