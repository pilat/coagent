package telegram

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/registry"
)

type assistantTelegramClient struct {
	delayedTelegramClient
	once    sync.Once
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int64
}

type busyAssistantClient struct {
	delayedTelegramClient
	once             sync.Once
	entered, release chan struct{}
}

func TestHarnessScenario_AssistantInProgressDoesNotBlockWorkOutput(t *testing.T) {
	client := &busyAssistantClient{entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-client.release:
		default:
			close(client.release)
		}
	})
	h := newDelayedTelegramHarnessWithClient(t, func(*config.Config) (llm.Client, error) { return client, nil })
	require.NoError(t, h.service.Start(t.Context()))
	manager := newAssistantTelegramManager(t, h)
	require.NoError(t, manager.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, manager.Stop(context.Background())) })
	manager.processUpdate(t.Context(), assistantTelegramUpdate("check for changes"))
	select {
	case <-client.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("assistant did not start")
	}

	id := h.produceBeforeManager(t)
	manager.processUpdate(t.Context(), assistantTelegramUpdate(fmt.Sprintf("/switch %d", id)))
	require.Eventually(t, func() bool {
		return hasTelegramMessage(h.recorder.snapshot(), "delayed telegram answer")
	}, 5*time.Second, 10*time.Millisecond)
	assert.False(t, hasTelegramMessage(h.recorder.snapshot(), "No change."))
	close(client.release)
	manager.processUpdate(t.Context(), assistantTelegramUpdate("/assistant"))
	require.Eventually(t, func() bool {
		return hasTelegramMessage(h.recorder.snapshot(), "No change.")
	}, 3*time.Second, 10*time.Millisecond)
}

func TestHarnessScenario_AssistantTelegram(t *testing.T) {
	client := &assistantTelegramClient{entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-client.release:
		default:
			close(client.release)
		}
	})
	h := newDelayedTelegramHarnessWithClient(t, func(*config.Config) (llm.Client, error) { return client, nil })
	require.NoError(t, h.service.Start(t.Context()))
	root, err := h.controller.EnsureAssistantRoot(t.Context())
	require.NoError(t, err)
	record, err := h.sessions.GetSession(t.Context(), root.SessionID)
	require.NoError(t, err)
	record.Attributes["preserved"] = "value"
	require.NoError(t, h.sessions.SetAttributes(t.Context(), root.SessionID, record.Attributes))

	manager := newAssistantTelegramManager(t, h)
	require.NoError(t, manager.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, manager.Stop(context.Background())) })
	awaitChatTail(t, h, root.SessionID)
	manager.processUpdate(t.Context(), assistantTelegramUpdate("hello"))
	select {
	case <-client.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("assistant did not receive input")
	}

	var accepted bool
	require.NoError(t, h.sessions.WithTx(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM session_inbox
			WHERE session_id = ? AND source = 'user' AND state = 'accepted' AND raw_content = 'hello')`,
			root.SessionID).Scan(&accepted)
	}))
	assert.True(t, accepted)
	require.Eventually(t, func() bool {
		return hasTelegramMethod(h.recorder.snapshot(), "sendChatAction")
	}, 3*time.Second, 10*time.Millisecond)
	close(client.release)
	require.Eventually(t, func() bool {
		return hasTelegramMessage(h.recorder.snapshot(), "Plain assistant reply")
	}, 3*time.Second, 10*time.Millisecond)
	awaitChatTail(t, h, root.SessionID)

	for _, command := range []string{"/kill", "/stop", "/clear", "/compact focus"} {
		manager.processUpdate(t.Context(), assistantTelegramUpdate(command))
	}
	manager.processUpdate(t.Context(), telegramUpdate{Message: &telegramMessage{
		From: &telegramUser{ID: 7}, Chat: telegramChat{ID: harnessChatID},
		Photo: []telegramPhotoSize{{FileID: "ignored-photo"}},
	}})
	manager.processUpdate(t.Context(), assistantTelegramUpdate("/model 1"))
	stored, err := h.sessions.GetSession(t.Context(), root.SessionID)
	require.NoError(t, err)
	assert.Equal(t, "fake-model", stored.Model)
	assert.Equal(t, "value", stored.Attributes["preserved"])
	assert.Equal(t, int64(2), client.calls.Load())

	var refused, replies int
	for _, call := range h.recorder.snapshot() {
		if call.Method == "sendMessage" && call.Text == "Plain assistant reply" {
			replies++
		}
		if strings.Contains(call.Text, "This command is unavailable") {
			refused++
		}
	}
	assert.Equal(t, 1, replies)
	assert.Equal(t, 4, refused)

	require.NoError(t, manager.Stop(context.Background()))
	second := newAssistantTelegramManager(t, h)
	require.NoError(t, second.Start(t.Context()))
	awaitChatTail(t, h, root.SessionID)
	require.NoError(t, second.Stop(context.Background()))
	assert.Equal(t, root.SessionID, second.assistantRootID)
	replies = 0
	for _, call := range h.recorder.snapshot() {
		if call.Method == "sendMessage" && call.Text == "Plain assistant reply" {
			replies++
		}
	}
	assert.Equal(t, 1, replies, "restart keeps the assistant watermark")
}

func TestHarnessScenario_AssistantAttachmentForwardsOnlyCaption(t *testing.T) {
	h := newDelayedTelegramHarness(t)
	root, err := h.controller.EnsureAssistantRoot(t.Context())
	require.NoError(t, err)
	manager := newAssistantTelegramManager(t, h)
	manager.processUpdate(t.Context(), telegramUpdate{Message: &telegramMessage{
		From: &telegramUser{ID: 7}, Chat: telegramChat{ID: harnessChatID}, Caption: "caption only",
		Photo: []telegramPhotoSize{{FileID: "never-download"}},
	}})
	var count int
	require.NoError(t, h.sessions.WithTx(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM session_inbox
			WHERE session_id = ? AND raw_content = 'caption only'`, root.SessionID).Scan(&count)
	}))
	assert.Equal(t, 1, count)
	assert.Empty(t, h.recorder.snapshot(), "caption-only forwarding needs no file API call or echo")
}

func (c *assistantTelegramClient) Chat(
	ctx context.Context,
	_ string,
	_ []llmwire.Message,
	_ []llmwire.ToolSchema,
	_ ...llmwire.ChatOption,
) (*llmwire.Response, error) {
	c.calls.Add(1)
	c.once.Do(func() { close(c.entered) })
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.release:
		return &llmwire.Response{
			Text:       "Plain assistant reply",
			FinishType: llmwire.FinishStop,
		}, nil
	}
}

func (c *busyAssistantClient) Chat(
	ctx context.Context,
	system string,
	messages []llmwire.Message,
	tools []llmwire.ToolSchema,
	options ...llmwire.ChatOption,
) (*llmwire.Response, error) {
	if strings.HasPrefix(system, registry.AssistantPrompt) {
		c.once.Do(func() { close(c.entered) })
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.release:
		}
		return &llmwire.Response{
			Text: "No change.", FinishType: llmwire.FinishStop,
		}, nil
	}

	return c.delayedTelegramClient.Chat(ctx, system, messages, tools, options...)
}

func newAssistantTelegramManager(t *testing.T, h *delayedTelegramHarness) *Manager {
	t.Helper()
	m, err := New(config.ManagerEntry{
		ID: delayedTelegramManagerID, BotToken: "test-token", AllowedUserIDs: []int64{7},
		TargetChatID: new(int64(harnessChatID)),
	}, nil, h.controller)
	require.NoError(t, err)
	m.httpClient = &http.Client{Transport: h.recorder}
	return m
}

func assistantTelegramUpdate(text string) telegramUpdate {
	return telegramUpdate{Message: &telegramMessage{
		From: &telegramUser{ID: 7}, Chat: telegramChat{ID: harnessChatID}, Text: text,
	}}
}
