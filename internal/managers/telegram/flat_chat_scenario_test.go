package telegram

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
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
	manager := newChatTelegramManager(t, h)
	require.NoError(t, manager.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, manager.Stop(context.Background())) })
	awaitChatTail(t, h, one)

	enqueueChatMessage(t, h, three, "third output")
	enqueueChatMessage(t, h, two, "new tail one")
	enqueueChatMessage(t, h, two, "new tail two")
	manager.processUpdate(t.Context(), chatTelegramUpdate(fmt.Sprintf("/switch %d", two)))
	awaitChatTail(t, h, two)
	var messages []string
	for _, call := range h.recorder.snapshot() {
		if call.Method == "sendMessage" {
			messages = append(messages, call.Text)
		}
	}
	assert.Equal(t, []string{
		fmt.Sprintf("→ #%d project", one), fmt.Sprintf("#%d project\nstartup", one), fmt.Sprintf("→ #%d project", two),
		fmt.Sprintf("#%d project\nnew tail one", two), fmt.Sprintf("#%d project\nnew tail two", two),
	}, messages)
	for _, text := range messages {
		assert.NotContains(t, text, fmt.Sprintf("#%d project", three))
	}
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
	require.NoError(t, chat.BindChat(t.Context(), strconv.FormatInt(harnessChatID, 10)))
	manager := newChatTelegramManager(t, h)
	require.NoError(t, manager.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, manager.Stop(context.Background())) })
	enqueueChatMessage(t, h, one, "first output")
	enqueueChatMessage(t, h, two, "second tail")
	manager.processUpdate(t.Context(), chatTelegramUpdate(fmt.Sprintf("/switch %d", two)))
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
	manager := newChatTelegramManager(t, h)
	require.NoError(t, manager.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, manager.Stop(context.Background())) })
	require.NoError(t, h.sessions.WithTx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(
			t.Context(),
			`INSERT INTO schedules(session_id,cron_expr,input_message) VALUES (?, '0 9 * * *','')`,
			work,
		)
		return err
	}))
	enqueueChatMessage(t, h, work, "scheduled work result")
	manager.processUpdate(t.Context(), chatTelegramUpdate("/sessions"))
	require.Eventually(t, func() bool {
		return hasTelegramMessage(
			h.recorder.snapshot(),
			fmt.Sprintf("#%d project · daily check ⏰", work),
		)
	}, time.Second, time.Millisecond)
	manager.processUpdate(t.Context(), chatTelegramUpdate(fmt.Sprintf("/switch %d", work)))
	awaitChatTail(t, h, work)
	assert.True(t, hasTelegramMessage(h.recorder.snapshot(), fmt.Sprintf("→ #%d project · daily check ⏰", work)))
	assert.True(
		t,
		hasTelegramMessage(
			h.recorder.snapshot(),
			fmt.Sprintf("#%d project · daily check\nscheduled work result", work),
		),
	)
}

func TestHarnessScenario_NumberedControlsAndTextKeepTheirRouting(t *testing.T) {
	h := newDelayedTelegramHarness(t)
	one := createChatWork(t, h)
	two := createChatWork(t, h)
	chat := h.controller.(controllerapi.ChatFocusController)
	key := strconv.FormatInt(harnessChatID, 10)
	require.NoError(t, chat.SetChatFocus(t.Context(), key, one))
	manager := newChatTelegramManager(t, h)
	manager.processUpdate(t.Context(), chatTelegramUpdate(fmt.Sprintf("/stop %d", two)))
	manager.processUpdate(t.Context(), chatTelegramUpdate(fmt.Sprintf("/kill %d", two)))
	manager.processUpdate(t.Context(), chatTelegramUpdate("plain focused text"))
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
	manager := newChatTelegramManager(t, h)
	require.NoError(t, manager.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, manager.Stop(context.Background())) })
	awaitChatTail(t, h, id)
	_, err := h.sessions.MarkSessionKilledWithOutput(t.Context(), id, 3)
	require.NoError(t, err)
	manager.wakeDelivery()
	require.Eventually(t, func() bool {
		focus, err := chat.ChatFocus(t.Context(), key)
		return err == nil && focus == 0
	}, 3*time.Second, 10*time.Millisecond)
	manager.processUpdate(t.Context(), chatTelegramUpdate(fmt.Sprintf("/switch %d", id)))
	var messages []string
	for _, call := range h.recorder.snapshot() {
		if call.Method == "sendMessage" {
			messages = append(messages, call.Text)
		}
	}
	assert.Equal(t, []string{
		fmt.Sprintf("→ #%d project", id),
		fmt.Sprintf("#%d project\nSession killed. Cancelled background processes: 3", id),
		fmt.Sprintf("#%d is not a live session", id),
	}, messages)
	focus, err := chat.ChatFocus(t.Context(), key)
	require.NoError(t, err)
	assert.Zero(t, focus)
}

func TestHarnessScenario_ForbiddenChatPausesWithoutMarking(t *testing.T) {
	h := newDelayedTelegramHarness(t)
	id := createChatWork(t, h)
	outputID := enqueueChatMessage(t, h, id, "must keep the cursor")
	chat := h.controller.(controllerapi.ChatFocusController)
	key := strconv.FormatInt(harnessChatID, 10)
	require.NoError(t, chat.SetChatFocus(t.Context(), key, id))
	entered, err := chat.EnterChat(t.Context(), key, id, 0)
	require.NoError(t, err)
	require.True(t, entered)
	h.recorder.sendResponses = []string{`{"ok":false,"error_code":403,"description":"bot blocked"}`}
	manager := newChatTelegramManager(t, h)
	require.NoError(t, manager.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, manager.Stop(context.Background())) })
	require.Eventually(t, func() bool {
		return hasTelegramMessage(h.recorder.snapshot(), fmt.Sprintf("#%d project\nmust keep the cursor", id))
	}, time.Second, time.Millisecond)
	require.NoError(t, manager.Stop(t.Context()))
	_, cursor, entered, err := chat.ChatCursor(t.Context(), key)
	require.NoError(t, err)
	assert.True(t, entered)
	assert.Less(t, cursor, outputID)
	rows, err := chat.ChatOutputs(t.Context(), key, id, cursor, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "must keep the cursor", rows[0].Content)
}

func TestHarnessScenario_ParseRejectionFallsBackBeforeMarking(t *testing.T) {
	h := newDelayedTelegramHarness(t)
	id := createChatWork(t, h)
	outputID := enqueueChatMessage(t, h, id, "**answer**")
	chat := h.controller.(controllerapi.ChatFocusController)
	key := strconv.FormatInt(harnessChatID, 10)
	require.NoError(t, chat.SetChatFocus(t.Context(), key, id))
	entered, err := chat.EnterChat(t.Context(), key, id, 0)
	require.NoError(t, err)
	require.True(t, entered)
	h.recorder.sendResponses = []string{`{"ok":false,"error_code":400,"description":"can't parse entities"}`}
	manager := newChatTelegramManager(t, h)
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
	_, cursor, entered, err := chat.ChatCursor(t.Context(), key)
	require.NoError(t, err)
	assert.True(t, entered)
	assert.Equal(t, outputID, cursor)
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

func newChatTelegramManager(t *testing.T, h *delayedTelegramHarness) *Manager {
	t.Helper()
	m, err := New(config.ManagerEntry{
		ID: delayedTelegramManagerID, BotToken: "test-token", AllowedUserIDs: []int64{harnessChatID},
	}, nil, h.controller)
	require.NoError(t, err)
	m.httpClient = &http.Client{Transport: h.recorder}
	return m
}

func chatTelegramUpdate(text string) telegramUpdate {
	return telegramUpdate{Message: &telegramMessage{
		From: &telegramUser{ID: harnessChatID}, Chat: telegramChat{ID: harnessChatID}, Text: text,
	}}
}
