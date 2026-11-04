package telegram

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
)

func TestHarnessScenario_NoFocusAnswersHintOnce(t *testing.T) {
	h := newDelayedTelegramHarness(t)
	m := newChatTelegramManager(t, h)
	m.cfg.Whisper = &config.ManagerWhisperEntry{Provider: "fixture", Model: "fixture"}
	m.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		method := req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]
		require.NotContains(t, []string{"getFile", "transcriptions"}, method)
		return h.recorder.RoundTrip(req)
	})}
	m.downloadClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("no-focus media must not be downloaded")
		return nil, fmt.Errorf("unexpected download")
	})}
	require.NoError(t, m.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, m.Stop(context.Background())) })
	for _, update := range []telegramUpdate{
		chatTelegramUpdate("plain text"),
		{Message: &telegramMessage{From: &telegramUser{ID: 7}, Chat: telegramChat{ID: harnessChatID}, Voice: &telegramVoiceMessage{FileID: "voice"}}},
		{Message: &telegramMessage{From: &telegramUser{ID: 7}, Chat: telegramChat{ID: harnessChatID}, Caption: "caption", Photo: []telegramPhotoSize{{FileID: "photo"}}}},
		{Message: &telegramMessage{From: &telegramUser{ID: 7}, Chat: telegramChat{ID: harnessChatID}, Document: &telegramDocument{FileID: "document"}}},
		{Message: &telegramMessage{From: &telegramUser{ID: 7}, Chat: telegramChat{ID: harnessChatID}, Video: &telegramVideo{FileID: "video"}}},
		{Message: &telegramMessage{From: &telegramUser{ID: 7}, Chat: telegramChat{ID: harnessChatID}, Caption: "audio caption", Audio: &telegramAudio{FileID: "audio"}}},
		chatTelegramUpdate("/status"), chatTelegramUpdate("/model"),
	} {
		before := len(chatMessages(h))
		m.processUpdate(t.Context(), update)
		require.Equal(t, []string{noFocusHint}, chatMessages(h)[before:])
	}
	before := len(chatMessages(h))
	m.processUpdate(t.Context(), chatTelegramUpdate("/help"))
	replies := chatMessages(h)[before:]
	require.Len(t, replies, 1)
	assert.Contains(t, replies[0], "Commands:")
	assert.NotContains(t, replies[0], "Skills:")
	assert.NotContains(t, replies[0], "No skills loaded.")
	m.processUpdate(t.Context(), chatTelegramUpdate("/sessions"))
	assert.Equal(t, "No sessions. /new or /spawn.", chatMessages(h)[before+1])
	var inputs, sessions int
	require.NoError(t, h.sessions.WithTx(t.Context(), func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM session_inbox`).Scan(&inputs); err != nil {
			return err
		}
		return tx.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sessions`).Scan(&sessions)
	}))
	assert.Zero(t, inputs)
	assert.Zero(t, sessions)
}

func TestHarnessScenario_KillFocusLeavesNoFocus(t *testing.T) {
	h := newDelayedTelegramHarness(t)
	require.NoError(t, h.service.Start(t.Context()))
	id := createChatWork(t, h)
	chat := h.controller.(controllerapi.ChatFocusController)
	key := strconv.FormatInt(harnessChatID, 10)
	require.NoError(t, chat.SetChatFocus(t.Context(), key, id))
	m := newChatTelegramManager(t, h)
	require.NoError(t, m.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, m.Stop(context.Background())) })
	awaitChatTail(t, h, id)
	m.processUpdate(t.Context(), chatTelegramUpdate("/kill"))
	require.Eventually(t, func() bool {
		focus, err := chat.ChatFocus(t.Context(), key)
		return err == nil && focus == 0
	}, 3*time.Second, time.Millisecond)
	messages := chatMessages(h)
	require.NotEmpty(t, messages)
	assert.Equal(
		t,
		fmt.Sprintf("#%d project\nSession killed. Cancelled background processes: 0", id),
		messages[len(messages)-1],
	)
	var selected sql.NullInt64
	require.NoError(t, h.sessions.WithTx(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT session_id FROM chat_focus WHERE manager_id = ? AND chat_id = ?`,
			delayedTelegramManagerID, key).Scan(&selected)
	}))
	assert.False(t, selected.Valid)
	m.wakeDelivery()
	m.processUpdate(t.Context(), chatTelegramUpdate("next text"))
	assert.Equal(t, append(messages, noFocusHint), chatMessages(h))
}

func TestHarnessScenario_ConfigCommandReachesWorkRoot(t *testing.T) {
	h := newDelayedTelegramHarness(t)
	id := createChatWork(t, h)
	chat := h.controller.(controllerapi.ChatFocusController)
	require.NoError(t, chat.SetChatFocus(t.Context(), strconv.FormatInt(harnessChatID, 10), id))
	m := newChatTelegramManager(t, h)
	m.processUpdate(t.Context(), chatTelegramUpdate("/config change the default model"))
	var content string
	require.NoError(t, h.sessions.WithTx(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT raw_content FROM session_inbox WHERE session_id = ?`, id).
			Scan(&content)
	}))
	assert.Equal(t, "/config change the default model", content)
	assert.Empty(t, chatMessages(h))
}

func chatMessages(h *delayedTelegramHarness) []string {
	var messages []string
	for _, call := range h.recorder.snapshot() {
		if call.Method == "sendMessage" {
			messages = append(messages, call.Text)
		}
	}
	return messages
}
