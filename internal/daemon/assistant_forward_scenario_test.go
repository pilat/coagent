package daemon

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestHarnessScenario_AssistantForwardsUserMessage(t *testing.T) {
	var work int64
	release := make(chan struct{})
	t.Cleanup(func() { closeOnce(release) })
	h := newHarness(t, harnessOptions{respond: func(system string, messages []llmwire.Message) *llmwire.Response {
		if !strings.HasPrefix(system, registry.AssistantPrompt) {
			<-release
			return textReply("work completed")
		}
		if hasUserContaining(messages, "answered:") || hasToolResultFor(messages, "forward") {
			return textReply("")
		}
		return callReply("forward-current", "forward", fmt.Sprintf(`{"session":"%d"}`, work))
	}})
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	attributes := managerAttrs(scenarioManagerID)
	attributes[sessionstore.TitleAttribute] = "work"
	target, err := h.store.CreateSession(h.ctx, h.projectID, "fake-model", "", attributes)
	require.NoError(t, err)
	work = target.ID
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, root.ID, "да, делай"))
	h.waitUntil("forward turn finished silently", func() bool {
		return assistantForwardReceiptCount(t, h, root.ID) == 1 && !h.mgr.HasActiveLoop(root.ID)
	})
	rows := assistantChatMessages(t, h, root.ID)
	require.Len(t, rows, 1)
	assert.Equal(t, fmt.Sprintf("→ #%d %s · work (forwarded)", work, filepathBaseWorkDir(t, h)), rows[0].Content)
	var source sessionstore.InputSource
	var words, rawAttributes string
	require.NoError(
		t,
		h.db.QueryRowContext(h.ctx, `SELECT source,raw_content,attributes FROM session_inbox WHERE session_id = ?`, work).
			Scan(&source, &words, &rawAttributes),
	)
	assert.Equal(t, "да, делай", words)
	var attrs map[string]any
	require.NoError(t, json.Unmarshal([]byte(rawAttributes), &attrs))
	assert.True(t, sessionstore.IsHumanInput(source, attrs))
	assert.NotContains(t, attrs, "from")
	assert.EqualValues(t, root.ID, attrs[sessionstore.ObserverSessionIDAttribute])
	closeOnce(release)
	waitAssistantObservation(t, h, root.ID, 1)
	rows = assistantChatMessages(t, h, root.ID)
	require.Len(t, rows, 2)
	assert.Equal(t, work, rows[1].Origin)
	var original string
	require.NoError(
		t,
		h.db.QueryRowContext(h.ctx, `SELECT content FROM session_outbox WHERE session_id = ? AND source_key LIKE 'message:%:final'`, work).
			Scan(&original),
	)
	assert.Equal(t, original, rows[1].Content)
	assert.Less(t, rows[0].ID, rows[1].ID)
	assertAssistantOwnOutputCount(t, h, root.ID, 0)
	record, err := h.store.GetSession(h.ctx, root.ID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.SessionStatusCompleted, record.Status)
	controller := newTestController(h.mgr, h.mgr.build.Config, nil, nil).ForManager(scenarioManagerID)
	chat, ok := controller.(controllerapi.ChatFocusController)
	require.True(t, ok)
	require.NoError(t, chat.BindChat(h.ctx, "chat"))
	sent := relayChatDelivery(t, h, chat, root.ID, rows[1].ID)
	assert.Equal(t, []string{rows[0].Content, fmt.Sprintf("#%d %s\n%s", work, rows[1].OriginName, original)}, sent)
}

func TestHarnessScenario_AssistantForwardRefusesSlash(t *testing.T) {
	assistantForwardRefusalScenario(t, "/budget 5", "", sessionstore.ErrForwardedCommand.Error())
}

func TestHarnessScenario_AssistantForwardRefusesParkedRoot(t *testing.T) {
	assistantForwardRefusalScenario(t, "continue", "parked", sessionstore.ErrBudgetParked.Error())
}

func TestHarnessScenario_AssistantSilentWhenFocusedOnWork(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: func(system string, _ []llmwire.Message) *llmwire.Response {
		if strings.HasPrefix(system, registry.AssistantPrompt) {
			return textReply("")
		}
		return textReply("ordinary work answer")
	}})
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	work, err := h.store.CreateSession(h.ctx, h.projectID, "fake-model", "", managerAttrs(scenarioManagerID))
	require.NoError(t, err)
	require.NoError(t, h.store.SetChatFocus(h.ctx, scenarioManagerID, "chat", work.ID))
	h.startInboxWake()
	require.NoError(t, assistantFor(h, root.ID).Send(h.ctx, work.ID, "do the work"))
	waitAssistantObservation(t, h, root.ID, 1)
	assert.Empty(t, assistantChatMessages(t, h, root.ID))
	assertAssistantOwnOutputCount(t, h, root.ID, 0)
	record, err := h.store.GetSession(h.ctx, root.ID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.SessionStatusCompleted, record.Status)
}

func assistantForwardRefusalScenario(t *testing.T, input, state, want string) {
	t.Helper()
	var work int64
	h := newHarness(t, harnessOptions{respond: func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasToolResultFor(messages, "forward") {
			return textReply("forward refused")
		}
		return callReply("forward-refused", "forward", fmt.Sprintf(`{"session":"%d"}`, work))
	}})
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	target, err := h.store.CreateSession(h.ctx, h.projectID, "fake-model", "", managerAttrs(scenarioManagerID))
	require.NoError(t, err)
	work = target.ID
	prepareAssistantCommandTarget(t, h, root.ID, work, state, nil)
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, root.ID, input))
	h.waitUntil("forward refusal reported", func() bool {
		return assistantSayCount(t, h, root.ID, "forward refused") == 1 && !h.mgr.HasActiveLoop(root.ID)
	})
	assert.Contains(t, configLastToolResultContent(h.messages(root.ID), "forward"), want)
	var inputs int
	require.NoError(
		t,
		h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM session_inbox WHERE session_id = ?`, work).Scan(&inputs),
	)
	assert.Zero(t, inputs)
	assert.Zero(t, assistantForwardReceiptCount(t, h, root.ID))
	if state == "parked" {
		record, err := h.store.Get(h.ctx, work)
		require.NoError(t, err)
		assert.Equal(t, "fired", string(record.State))
	}
}

func assistantForwardReceiptCount(t *testing.T, h *harness, root int64) int {
	t.Helper()
	var count int
	require.NoError(
		t,
		h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM session_outbox WHERE session_id = ? AND source_key LIKE 'forward:%'`, root).
			Scan(&count),
	)
	return count
}

func assistantChatMessages(t *testing.T, h *harness, root int64) []sessionstore.ChatOutput {
	t.Helper()
	rows, err := h.store.ChatOutputs(h.ctx, scenarioManagerID, "chat", root, 100)
	require.NoError(t, err)
	var outputs []sessionstore.ChatOutput
	for _, row := range rows {
		if row.Type == sessionstore.OutputMessage {
			outputs = append(outputs, row)
		}
	}
	return outputs
}

func waitAssistantObservation(t *testing.T, h *harness, root int64, want int) {
	t.Helper()
	h.waitUntil("assistant observation completed", func() bool {
		var accepted int
		err := h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM session_inbox WHERE session_id = ? AND source = 'agent'
			AND state = 'accepted' AND json_extract(attributes, '$.observed_kind') IS NOT NULL`, root).Scan(&accepted)
		require.NoError(t, err)
		if accepted != want || h.mgr.HasActiveLoop(root) {
			return false
		}
		record, err := h.store.GetSession(h.ctx, root)
		require.NoError(t, err)
		require.Equal(t, sessionstore.SessionStatusCompleted, record.Status)
		return true
	})
}

func assertAssistantOwnOutputCount(t *testing.T, h *harness, root int64, want int) {
	t.Helper()
	var count int
	require.NoError(
		t,
		h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM session_outbox WHERE session_id = ? AND source_key LIKE 'message:%'`, root).
			Scan(&count),
	)
	assert.Equal(t, want, count)
}
