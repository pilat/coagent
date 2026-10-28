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
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/sessionstore"
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
	topic, ok := manager.getTopicBySessionID(manager.assistantRootID)
	require.True(t, ok)
	manager.processUpdate(t.Context(), assistantTelegramUpdate(topic, "check for changes"))
	select {
	case <-client.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("assistant did not start")
	}
	h.produceBeforeManager(t)
	require.Eventually(
		t,
		func() bool { return h.recorder.hasMessagePrefix("delayed telegram answer\n\n") },
		5*time.Second,
		10*time.Millisecond,
	)
	awaitAssistantQueue(t, h)
	assert.False(
		t,
		hasTelegramMessage(h.recorder.snapshot(), "No change."),
		"work output arrives while the assistant is thinking",
	)
	close(client.release)
	require.Eventually(
		t,
		func() bool { return hasTelegramMessage(h.recorder.snapshot(), "No change.") },
		3*time.Second,
		10*time.Millisecond,
	)
	assert.False(t, hasTelegramMessage(h.recorder.snapshot(), ""))
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
	h := newDelayedTelegramHarnessWithClient(t, func(cfg *config.Config) (llm.Client, error) {
		cfg.UnifiedConfig = &config.UnifiedConfig{Managers: []config.ManagerEntry{
			{ID: delayedTelegramManagerID, AssistantTopicName: "My desk"},
		}}

		return client, nil
	})
	require.NoError(t, h.service.Start(t.Context()))
	root, err := h.controller.EnsureAssistantRoot(t.Context())
	require.NoError(t, err)
	root.Attributes["preserved"] = "value"
	require.NoError(t, h.controller.SetSessionAttributes(t.Context(), controllerapi.SessionSetAttributesData{
		SessionID: root.SessionID, Attributes: root.Attributes,
	}))
	manager := newAssistantTelegramManager(t, h)
	require.NoError(t, manager.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, manager.Stop(context.Background())) })
	topic, ok := manager.getTopicBySessionID(root.SessionID)
	require.True(t, ok)
	require.Positive(t, topic)
	stored, err := h.sessions.GetSession(t.Context(), root.SessionID)
	require.NoError(t, err)
	assert.Equal(t, "value", stored.Attributes["preserved"])
	assert.Equal(t, delayedTelegramManagerID, stored.Attributes[controllerapi.SessionAttributeManagerID])
	awaitAssistantQueue(t, h)

	manager.processUpdate(t.Context(), assistantTelegramUpdate(topic, "hello"))
	select {
	case <-client.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("assistant did not receive the topic input")
	}
	inputs, err := h.sessions.ListPending(t.Context(), root.SessionID)
	require.NoError(t, err)
	assert.Empty(t, inputs, "the ordinary boundary accepts input before the first model call")
	var accepted bool
	require.NoError(t, h.sessions.WithTx(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM session_inbox WHERE session_id = ? AND source = 'user' AND state = 'accepted' AND raw_content = 'hello')`, root.SessionID).
			Scan(&accepted)
	}))
	assert.True(t, accepted)
	require.Eventually(t, func() bool {
		for _, call := range h.recorder.snapshot() {
			if call.Method == "sendChatAction" && call.ThreadID == topic {
				return true
			}
		}
		return false
	}, 3*time.Second, 10*time.Millisecond, "the blocked model call must produce an ordinary heartbeat")
	close(client.release)
	require.Eventually(t, func() bool { return hasTelegramMessage(h.recorder.snapshot(), "Plain assistant reply") },
		3*time.Second, 10*time.Millisecond, "delivery must wake from the notification, before the one-minute timer")
	awaitAssistantQueue(t, h)

	for _, command := range []string{commandKill, commandStop, commandClear, "/compact focus"} {
		manager.processUpdate(t.Context(), assistantTelegramUpdate(topic, command))
	}
	manager.processUpdate(t.Context(), telegramUpdate{Message: &telegramMessage{
		From: &telegramUser{ID: 7}, Chat: telegramChat{ID: harnessChatID}, MessageThreadID: topic,
		Photo: []telegramPhotoSize{{FileID: "ignored-photo"}},
	}})
	manager.handleModel(t.Context(), root.SessionID, topic)
	manager.handleCallbackModel(
		t.Context(),
		&telegramCallbackData{ID: "model", Message: &telegramCallbackMeta{MessageID: 123}},
		callbackAction{ModelID: "fake-model"},
		topic,
	)
	stored, err = h.sessions.GetSession(t.Context(), root.SessionID)
	require.NoError(t, err)
	assert.Equal(t, "fake-model", stored.Model)
	assert.Equal(t, int64(2), client.calls.Load(), "the ordinary completion nudge confirms the first answer")
	var refused, replies, typing int
	for _, call := range h.recorder.snapshot() {
		if call.Method == "sendChatAction" && call.ThreadID == topic {
			typing++
		}
		if call.Method == "sendMessage" && call.Text == "Plain assistant reply" {
			replies++
			assert.Equal(t, topic, call.ThreadID)
			assert.NotContains(t, call.Text, "🤖")
		}
		if strings.Contains(call.Text, "This command is unavailable") {
			refused++
		}
	}
	assert.Equal(t, 1, replies)
	assert.Equal(t, 4, refused)
	assert.Positive(t, typing, "the ordinary assistant runner emits typing in its topic")
	assertAssistantKillPicker(t, h, manager, root.SessionID)
	require.NoError(t, manager.Stop(context.Background()))
	before := assistantTopicCreations(h.recorder.snapshot())
	second := newAssistantTelegramManager(t, h)
	require.NoError(t, second.Start(t.Context()))
	awaitAssistantQueue(t, h)
	require.NoError(t, second.Stop(context.Background()))
	assert.Equal(t, before, assistantTopicCreations(h.recorder.snapshot()))
	assert.Equal(t, root.SessionID, second.assistantRootID)
	assertAssistantTopicRepair(t, h, topic)
}

func TestAssistantTopicBindingPreservesAttributes(t *testing.T) {
	controller := &fakeController{assistantRoot: &controllerapi.AssistantRootEnsureData{
		SessionID: 42, Attributes: map[string]any{"manager_id": "telegram-main", "keep": "value"},
	}}
	var calls []telegramHarnessCall
	manager := newTelegramHarnessManager(t, controller, &calls)
	require.NoError(t, manager.ensureAssistantRoot(t.Context()))
	require.NoError(t, manager.ensureAssistantRoot(t.Context()))
	assert.Equal(t, "value", controller.lastSetAttrs.Attributes["keep"])
	assert.Equal(t, "telegram-main", controller.lastSetAttrs.Attributes["manager_id"])
	require.Len(t, calls, 1)
	assert.Equal(t, "createForumTopic", calls[0].Method)
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
		ID: delayedTelegramManagerID, BotToken: "test", AllowedUserIDs: []int64{7},
		TargetChatID: new(int64(harnessChatID)), ServiceTopicName: "Coagent", AssistantTopicName: "My desk",
	}, nil, h.controller)
	require.NoError(t, err)
	m.httpClient = &http.Client{Transport: h.recorder}

	return m
}

func assistantTelegramUpdate(topic int64, text string) telegramUpdate {
	return telegramUpdate{Message: &telegramMessage{
		From: &telegramUser{ID: 7}, Chat: telegramChat{ID: harnessChatID},
		MessageThreadID: topic, Text: text,
	}}
}

func awaitAssistantQueue(t *testing.T, h *delayedTelegramHarness) {
	t.Helper()
	require.Eventually(t, func() bool {
		queue, err := h.sessions.OutputQueueStatus(t.Context(), delayedTelegramManagerID)
		return err == nil && queue.Pending == 0
	}, 5*time.Second, 10*time.Millisecond)
}

func assistantTopicCreations(calls []delayedTelegramCall) int {
	var count int
	for _, call := range calls {
		if call.Method == "createForumTopic" && call.Name == "My desk" {
			count++
		}
	}

	return count
}

func assertAssistantKillPicker(t *testing.T, h *delayedTelegramHarness, m *Manager, root int64) {
	t.Helper()
	project, err := h.sessions.GetOrCreateProject(t.Context(), h.workDir)
	require.NoError(t, err)
	work, _, err := h.sessions.CreateManagerRoot(t.Context(), sessionstore.ManagerRootCreate{
		ProjectID: project, Model: "fake-model", Name: "work", WorkDir: h.workDir,
		Attributes: map[string]any{"manager_id": delayedTelegramManagerID},
	})
	require.NoError(t, err)
	m.wakeDelivery()
	awaitAssistantQueue(t, h)
	m.handleKill(t.Context(), 0, m.serviceTopicID)
	calls := h.recorder.snapshot()
	last := calls[len(calls)-1]
	assert.Contains(t, last.Markup, fmt.Sprintf("kill:%d", work.ID))
	assert.NotContains(t, last.Markup, fmt.Sprintf("kill:%d", root))
	m.handleCallbackKill(t.Context(), &telegramCallbackData{ID: "forged"}, callbackAction{Session: root})
	assert.Equal(t, "The assistant cannot be killed", h.recorder.snapshot()[len(calls)].Text)
}

func assertAssistantTopicRepair(t *testing.T, h *delayedTelegramHarness, oldTopic int64) {
	t.Helper()
	h.recorder.mu.Lock()
	h.recorder.missingTopicID = oldTopic
	h.recorder.mu.Unlock()
	m := newAssistantTelegramManager(t, h)
	require.NoError(t, m.Start(t.Context()))
	awaitAssistantQueue(t, h)
	require.NoError(t, m.Stop(context.Background()))
	assert.Equal(t, 2, assistantTopicCreations(h.recorder.snapshot()))
	record, err := h.sessions.GetSession(t.Context(), m.assistantRootID)
	require.NoError(t, err)
	topic, ok := topicIDFromAttributes(record.Attributes)
	assert.True(t, ok)
	assert.NotEqual(t, oldTopic, topic)
	assert.Equal(t, "value", record.Attributes["preserved"])
	var name string
	require.NoError(t, h.sessions.WithTx(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT json_extract(attributes, '$.name') FROM session_outbox
			WHERE session_id = ? AND type = 'session_opened' ORDER BY id DESC LIMIT 1`, m.assistantRootID).Scan(&name)
	}))
	assert.Equal(t, "My desk", name)
}
