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

func TestHarnessScenario_SwitchDeliversHeaderAndTail(t *testing.T) {
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
		fmt.Sprintf("#%d project\nstartup", one), fmt.Sprintf("→ #%d project", two),
		fmt.Sprintf("#%d project\nnew tail one", two), fmt.Sprintf("#%d project\nnew tail two", two),
	}, messages)
	seen, err := h.sessions.ChatSeen(t.Context(), delayedTelegramManagerID, strconv.FormatInt(harnessChatID, 10), three)
	require.NoError(t, err)
	assert.Zero(t, seen)
}

func TestHarnessScenario_SwitchShowsSessionTitles(t *testing.T) {
	h := newDelayedTelegramHarness(t)
	one := createChatWork(t, h)
	two := createChatWork(t, h)
	for _, work := range []struct {
		id    int64
		title string
	}{{one, "first work"}, {two, "second work"}} {
		_, err := h.sessions.Enqueue(t.Context(), sessionstore.Input{
			SessionID: work.id, Source: sessionstore.InputSourceUser, Content: work.title,
		})
		require.NoError(t, err)
	}
	chat := h.controller.(controllerapi.ChatFocusController)
	require.NoError(t, chat.SetChatFocus(t.Context(), strconv.FormatInt(harnessChatID, 10), 0))
	manager := newAssistantTelegramManager(t, h)
	require.NoError(t, manager.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, manager.Stop(context.Background())) })
	awaitChatTail(t, h, manager.assistantRootID)
	enqueueChatMessage(t, h, one, "first unread")
	enqueueChatMessage(t, h, two, "second tail")
	manager.processUpdate(t.Context(), assistantTelegramUpdate(fmt.Sprintf("/switch %d", two)))
	awaitChatTail(t, h, two)
	var messages []string
	for _, call := range h.recorder.snapshot() {
		if call.Method == "sendMessage" {
			messages = append(messages, call.Text)
		}
	}
	assert.Equal(t, []string{
		fmt.Sprintf("→ #%d project · second work", two),
		fmt.Sprintf("#%d project · second work\nsecond tail", two),
	}, messages)
}

func TestHarnessScenario_ScheduledSessionMarkers(t *testing.T) {
	h := newDelayedTelegramHarness(t)
	work := createChatWork(t, h)
	require.NoError(t, h.sessions.SetSessionTitle(t.Context(), delayedTelegramManagerID, work, "daily check"))
	manager := newAssistantTelegramManager(t, h)
	require.NoError(t, manager.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, manager.Stop(context.Background())) })
	awaitChatTail(t, h, manager.assistantRootID)
	require.NoError(t, h.sessions.WithTx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(
			t.Context(),
			`INSERT INTO schedules(session_id,cron_expr,input_message) VALUES (?, '0 9 * * *',''), (?, '0 9 * * *','')`,
			work,
			manager.assistantRootID,
		)
		return err
	}))
	enqueueChatMessage(t, h, work, "scheduled work result")
	manager.processUpdate(t.Context(), assistantTelegramUpdate("/sessions"))
	require.Eventually(t, func() bool {
		return hasTelegramMessage(
			h.recorder.snapshot(),
			fmt.Sprintf("#0 Assistant ⏰ ◀\n#%d project · daily check ⏰ · 1 new", work),
		)
	}, time.Second, time.Millisecond)
	manager.processUpdate(t.Context(), assistantTelegramUpdate(fmt.Sprintf("/switch %d", work)))
	awaitChatTail(t, h, work)
	assert.True(t, hasTelegramMessage(h.recorder.snapshot(), fmt.Sprintf("→ #%d project · daily check ⏰", work)))
	assert.True(
		t,
		hasTelegramMessage(
			h.recorder.snapshot(),
			fmt.Sprintf("#%d project · daily check\nscheduled work result", work),
		),
	)
	manager.processUpdate(t.Context(), assistantTelegramUpdate("/assistant"))
	require.Eventually(
		t,
		func() bool { return hasTelegramMessage(h.recorder.snapshot(), "→ #0 Assistant ⏰") },
		time.Second,
		time.Millisecond,
	)
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
		fmt.Sprintf("#%d project\nSession killed. Cancelled background processes: 3", id), "→ #0 Assistant",
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
		return hasTelegramMessage(h.recorder.snapshot(), fmt.Sprintf("#%d project\nmust remain unread", id))
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
		{
			Method:    "sendMessage",
			Text:      fmt.Sprintf("#%d project\n<b>answer</b>", id),
			ChatID:    harnessChatID,
			ParseMode: tgParseModeHTML,
		},
		{Method: "sendMessage", Text: fmt.Sprintf("#%d project\nanswer", id), ChatID: harnessChatID},
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
