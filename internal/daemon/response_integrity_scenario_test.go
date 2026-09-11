package daemon

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestHarnessScenario_LengthAttemptIsDiscardedBeforeToolExecution(t *testing.T) {
	h := newSubagentHarnessWith(t, lengthRecoveryResponder(t))
	collector := collectEvents(h.mgr.PubSub().SubscribeAll())
	defer func() {
		collector.stop()
		h.shutdown()
	}()

	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "exercise response recovery", "fake-model", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)
	waitForVisibleMessage(t, collector, sessionID, "recovered complete answer")
	drainScenarioClaims(t, "model_response_length_recovery.json", newChainController(t, h))
	waitForIdleAfterMessage(t, collector, sessionID, "recovered complete answer")

	messages := transcriptOf(h, sessionID)
	assert.Zero(t, countToolResultsFor(messages, "todowrite"))
	assert.Zero(t, countToolResultsFor(messages, "bash"))
	assert.NotContains(t, scenarioTranscriptText(messages), "rejected private fragment")
	assert.NotContains(t, strings.Join(visibleEventMessages(collector.snapshot(), sessionID), "\n"),
		sessionstore.OutputLengthRecoveryPrompt)

	var todoItems string
	require.NoError(t, h.db.QueryRowContext(h.ctx,
		`SELECT todo_items FROM sessions WHERE id = ?`, sessionID).Scan(&todoItems))
	assert.JSONEq(t, `[]`, todoItems, "the valid side-effect probe before the truncated call never executes")

	assertHarnessTrace(t, "model_response_length_recovery.json", collector.snapshot(), sessionID)
}

func lengthRecoveryResponder(t *testing.T) func(string, []llmwire.Message) *llmwire.Response {
	t.Helper()
	var calls int

	return func(_ string, messages []llmwire.Message) *llmwire.Response {
		calls++
		if calls > 1 {
			visible := scenarioTranscriptText(messages)
			require.NotContains(t, visible, "rejected private fragment")
			require.Contains(t, visible, sessionstore.OutputLengthRecoveryPrompt)

			return &llmwire.Response{Text: "recovered complete answer", FinishType: llmwire.FinishStop}
		}

		return &llmwire.Response{
			Text: "rejected private fragment", FinishType: llmwire.FinishLength,
			ProviderFinishReason: "length", CostUSD: 0.5,
			Usage: &llmwire.MessageUsage{PromptTokens: 100, CompletionTokens: 200},
			ToolCalls: []llmwire.ToolCall{
				{
					ID: "side-effect-probe", Name: "todowrite",
					Arguments: []byte(
						`{"items":[{"id":"must-not-run","content":"must not run","status":"in_progress","priority":"high"}]}`,
					),
				},
				{ID: "truncated-call", Name: "bash", Arguments: []byte(
					`{"command":"` + strings.Repeat("x", 128*1024),
				)},
			},
		}
	}
}

func TestHarnessScenario_RepeatedLengthPublishesCanonicalError(t *testing.T) {
	h := newSubagentHarnessWith(t, func(_ string, _ []llmwire.Message) *llmwire.Response {
		return &llmwire.Response{Text: "discarded", FinishType: llmwire.FinishLength}
	})
	collector := collectEvents(h.mgr.PubSub().SubscribeAll())
	defer func() {
		collector.stop()
		h.shutdown()
	}()

	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "repeat the limit", "fake-model", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)
	want := sessionstore.IntegrityErrorNotice(sessionstore.OutputLengthTerminalError)
	waitForVisibleMessage(t, collector, sessionID, want)
	drainScenarioClaims(t, "model_response_length_exhausted.json", newChainController(t, h))
	// waitIdle is not enough: teardown publishes the terminal idle after the
	// runner deregisters, so the trace must wait for the event itself.
	waitForIdleAfterMessage(t, collector, sessionID, want)

	assert.NotContains(t, strings.Join(visibleEventMessages(collector.snapshot(), sessionID), "\n"), "discarded")
	assertHarnessTrace(t, "model_response_length_exhausted.json", collector.snapshot(), sessionID)
}

func TestHarnessScenario_UnknownFinishPublishesCanonicalError(t *testing.T) {
	h := newSubagentHarnessWith(t, func(_ string, _ []llmwire.Message) *llmwire.Response {
		return &llmwire.Response{
			Text: "filtered partial", FinishType: llmwire.FinishUnknown,
			ProviderFinishReason: "content_filter",
		}
	})
	collector := collectEvents(h.mgr.PubSub().SubscribeAll())
	defer func() {
		collector.stop()
		h.shutdown()
	}()

	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "unknown finish", "fake-model", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)
	want := sessionstore.IntegrityErrorNotice(sessionstore.UnknownFinishTerminalError)
	waitForVisibleMessage(t, collector, sessionID, want)
	drainScenarioClaims(t, "model_response_unknown_finish.json", newChainController(t, h))
	waitForIdleAfterMessage(t, collector, sessionID, want)

	assert.NotContains(t, strings.Join(visibleEventMessages(collector.snapshot(), sessionID), "\n"), "filtered partial")
	assertHarnessTrace(t, "model_response_unknown_finish.json", collector.snapshot(), sessionID)
}

func scenarioTranscriptText(messages []llmwire.Message) string {
	var text strings.Builder
	for _, message := range messages {
		text.WriteString(message.Content)
		for _, call := range message.ToolCalls {
			text.Write(call.Arguments)
		}
	}

	return text.String()
}

func visibleEventMessages(events []controllerapi.SessionNotification, sessionID int64) []string {
	var messages []string
	for _, event := range events {
		if event.SessionID == sessionID && event.Notification.Message != "" {
			messages = append(messages, event.Notification.Message)
		}
	}

	return messages
}
