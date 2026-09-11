package session

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/transcript"
)

func TestRunLoopLengthRecoveryHidesRejectedAttemptAndPublishesSuccessOnce(t *testing.T) {
	db, store, sessionID := newFinalOutputStore(t)
	notifier := &loopNotifier{}
	probe := &countingTool{id: "probe"}
	model := &loopScriptLLM{onCall: func(call int, messages []llmwire.Message) (*llmwire.Response, error) {
		if call == 1 {
			return &llmwire.Response{
				Text: "partial secret", FinishType: llmwire.FinishLength, ProviderFinishReason: "length",
				ToolCalls: []llmwire.ToolCall{{ID: "partial", Name: "probe", Arguments: []byte(`{"bad":`)}},
				CostUSD:   0.25, Usage: &llmwire.MessageUsage{PromptTokens: 10, CompletionTokens: 20},
			}, nil
		}

		providerText := transcriptText(messages)
		assert.NotContains(t, providerText, "partial secret")
		assert.NotContains(t, providerText, `{"bad":`)
		assert.Contains(t, providerText, sessionstore.OutputLengthRecoveryPrompt)

		return textResponse("complete answer"), nil
	}}

	agent := newTestAgent(probe)
	agent.id, agent.rootID, agent.store = sessionID, sessionID, store
	agent.ms = newMessageStore(store, sessionID, store)
	agent.llmClient = model
	require.NoError(t, agent.ms.addUserMessage(t.Context(), "do the work"))

	result, err := runLoop(t.Context(), agent, loopOptions{Notify: notifier.fn}, iterationGuard(5))
	require.NoError(t, err)
	assert.Equal(t, "complete answer", result.FinalResponse)
	assert.Zero(t, probe.runs.Load())
	assert.Equal(t, []string{"complete answer"}, notifier.all())

	rows := loadIntegrityRows(t, db, sessionID)
	require.Len(t, rows, 4)
	assert.Equal(t, sessionstore.RejectedReasonOutputLength, rows[1].rejectedReason.String)
	assert.Equal(t, rows[1].id, rows[2].retryOf.Int64)
	assert.Equal(t, sessionstore.OutputLengthRecoveryPrompt, rows[2].content)
	assert.Equal(t, llmwire.FinishStop, rows[3].finishType.String)
}

func TestRunLoopRepeatedLengthCommitsOneTerminalError(t *testing.T) {
	db, store, sessionID := newFinalOutputStore(t)
	notifier := &loopNotifier{}
	model := &loopScriptLLM{responses: []*llmwire.Response{
		{Text: "first partial", FinishType: llmwire.FinishLength},
		{Text: "second partial", FinishType: llmwire.FinishLength},
	}}

	agent := newTestAgent()
	agent.id, agent.rootID, agent.store = sessionID, sessionID, store
	agent.ms = newMessageStore(store, sessionID, store)
	agent.llmClient = model
	require.NoError(t, agent.ms.addUserMessage(t.Context(), "do the work"))

	result, err := runLoop(t.Context(), agent, loopOptions{Notify: notifier.fn}, iterationGuard(5))
	require.EqualError(t, err, sessionstore.IntegrityErrorNotice(sessionstore.OutputLengthTerminalError))
	assert.Equal(t, sessionstore.IntegrityErrorNotice(sessionstore.OutputLengthTerminalError), result.ErrorNotice)
	assert.True(t, result.TerminalStateCommitted)
	assert.Empty(t, notifier.all())

	rows := loadIntegrityRows(t, db, sessionID)
	require.Len(t, rows, 4)
	assert.Equal(t, sessionstore.RejectedReasonOutputLength, rows[1].rejectedReason.String)
	assert.Equal(t, sessionstore.RejectedReasonOutputLength, rows[3].rejectedReason.String)
	assert.Equal(t, rows[1].id, rows[2].retryOf.Int64)

	var status string
	var iteration int
	require.NoError(t, db.QueryRowContext(t.Context(),
		`SELECT status, iteration FROM sessions WHERE id = ?`, sessionID).Scan(&status, &iteration))
	assert.Equal(t, string(sessionstore.SessionStatusError), status)
	assert.Equal(t, 2, iteration)
}

func TestRunLoopUnknownFinishRejectsWithoutRetry(t *testing.T) {
	db, store, sessionID := newFinalOutputStore(t)
	model := &loopScriptLLM{responses: []*llmwire.Response{{
		Text: "filtered partial", FinishType: llmwire.FinishUnknown, ProviderFinishReason: "content_filter",
	}}}

	agent := newTestAgent()
	agent.id, agent.rootID, agent.store = sessionID, sessionID, store
	agent.ms = newMessageStore(store, sessionID, store)
	agent.llmClient = model
	require.NoError(t, agent.ms.addUserMessage(t.Context(), "do the work"))

	result, err := runLoop(t.Context(), agent, loopOptions{}, iterationGuard(5))
	require.EqualError(t, err, sessionstore.IntegrityErrorNotice(sessionstore.UnknownFinishTerminalError))
	assert.Equal(t, 1, model.calls)
	assert.Equal(t, sessionstore.IntegrityErrorNotice(sessionstore.UnknownFinishTerminalError), result.ErrorNotice)

	rows := loadIntegrityRows(t, db, sessionID)
	require.Len(t, rows, 2)
	assert.Equal(t, sessionstore.RejectedReasonUnknownFinish, rows[1].rejectedReason.String)
	assert.Equal(t, "content_filter", rows[1].providerReason.String)
}

func TestRunLoopUnknownFinishNeverExecutesIncludedCalls(t *testing.T) {
	_, store, sessionID := newFinalOutputStore(t)
	probe := &countingTool{id: "probe"}
	agent := newTestAgent(probe)
	agent.id, agent.rootID, agent.store = sessionID, sessionID, store
	agent.ms = newMessageStore(store, sessionID, store)
	agent.llmClient = &loopScriptLLM{responses: []*llmwire.Response{{
		FinishType: llmwire.FinishUnknown,
		ToolCalls:  []llmwire.ToolCall{{ID: "rejected", Name: "probe", Arguments: []byte(`{}`)}},
	}}}
	require.NoError(t, agent.ms.addUserMessage(t.Context(), "do the work"))

	_, err := runLoop(t.Context(), agent, loopOptions{}, iterationGuard(3))
	require.Error(t, err)
	assert.Zero(t, probe.runs.Load())
}

func TestRunLoopStopWithCallsRetainsStructuralToolRouting(t *testing.T) {
	probe := &countingTool{id: "probe"}
	model := &loopScriptLLM{onCall: func(call int, _ []llmwire.Message) (*llmwire.Response, error) {
		if call == 1 {
			return &llmwire.Response{
				FinishType: llmwire.FinishStop,
				ToolCalls:  []llmwire.ToolCall{{ID: "accepted", Name: "probe", Arguments: []byte(`{}`)}},
			}, nil
		}

		return textResponse("done"), nil
	}}
	agent := newTestAgent(probe)
	agent.llmClient = model

	result, err := runLoop(t.Context(), agent, loopOptions{}, iterationGuard(5))
	require.NoError(t, err)
	assert.Equal(t, "done", result.FinalResponse)
	assert.Equal(t, int64(1), probe.runs.Load())
}

func TestRunLoopToolCallsFinishWithoutCallsUsesEmptyNudge(t *testing.T) {
	model := &loopScriptLLM{onCall: func(call int, messages []llmwire.Message) (*llmwire.Response, error) {
		if call == 1 {
			return &llmwire.Response{Text: "must stay hidden", FinishType: llmwire.FinishToolCalls}, nil
		}
		assert.Contains(t, transcriptText(messages), "must stay hidden")
		assert.Contains(t, transcriptText(messages), "You returned an empty response")

		return textResponse("done"), nil
	}}
	notifier := &loopNotifier{}
	agent := newTestAgent()
	agent.llmClient = model

	result, err := runLoop(t.Context(), agent, loopOptions{Notify: notifier.fn}, iterationGuard(5))
	require.NoError(t, err)
	assert.Equal(t, "done", result.FinalResponse)
	assert.Equal(t, []string{"done"}, notifier.all())
}

func TestRunLoopToolCallsFinishWithoutCallsUsesBoundedEmptyLimit(t *testing.T) {
	notifier := &loopNotifier{}
	agent := newTestAgent()
	agent.llmClient = &loopScriptLLM{responses: []*llmwire.Response{{
		Text: "hidden on every attempt", FinishType: llmwire.FinishToolCalls,
	}}}

	result, err := runLoop(t.Context(), agent, loopOptions{Notify: notifier.fn}, iterationGuard(10))
	require.NoError(t, err)
	assert.False(t, result.Suspended)
	assert.Empty(t, result.FinalResponse)
	assert.Equal(t, 1, notifier.countWith("consecutive empty responses"))
	assert.Zero(t, notifier.countWith("hidden on every attempt"))
}

func TestRejectedAttemptIsAbsentFromReloadedCompactionInput(t *testing.T) {
	_, store, sessionID := newFinalOutputStore(t)
	ms := newMessageStore(store, sessionID, nil)
	require.NoError(t, ms.addUserMessage(t.Context(), "task"))

	_, err := store.CommitRejectedResponse(t.Context(), sessionstore.RejectedResponse{
		SessionID: sessionID, RootID: sessionID, Iteration: 1,
		Message: &transcript.Message{
			Role: llmwire.RoleAssistant, Content: "rejected compaction text",
			FinishType: llmwire.FinishLength, RejectedReason: sessionstore.RejectedReasonOutputLength,
		},
	})
	require.NoError(t, err)
	require.NoError(t, ms.reloadMessages(t.Context()))

	serialized, err := serializeCanonical(ms.getMessages())
	require.NoError(t, err)
	assert.NotContains(t, serialized, "rejected compaction text")
	assert.Contains(t, serialized, sessionstore.OutputLengthRecoveryPrompt)
}

type integrityRow struct {
	id             int64
	content        string
	finishType     sql.NullString
	providerReason sql.NullString
	rejectedReason sql.NullString
	retryOf        sql.NullInt64
}

func loadIntegrityRows(t *testing.T, db *sql.DB, sessionID int64) []integrityRow {
	t.Helper()

	rows, err := db.QueryContext(t.Context(), `SELECT id, COALESCE(content, ''), finish_type,
		provider_finish_reason, rejected_reason, retry_of_message_id
		FROM messages WHERE session_id = ? ORDER BY id`, sessionID)
	require.NoError(t, err)
	defer rows.Close()

	var result []integrityRow
	for rows.Next() {
		var row integrityRow
		require.NoError(t, rows.Scan(
			&row.id, &row.content, &row.finishType, &row.providerReason, &row.rejectedReason, &row.retryOf,
		))
		result = append(result, row)
	}
	require.NoError(t, rows.Err())

	return result
}

func transcriptText(messages []llmwire.Message) string {
	var result strings.Builder
	for _, message := range messages {
		result.WriteString(message.Content)
		for _, call := range message.ToolCalls {
			result.Write(call.Arguments)
		}
	}

	return result.String()
}
