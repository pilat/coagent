package sessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/transcript"
)

func TestResponseIntegrityStoreRecoveryIsDurableAndExcluded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	root, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "test"})
	require.NoError(t, err)
	input, err := store.EnqueueInput(ctx, root.ID, InputSourceUser, "task")
	require.NoError(t, err)
	_, err = store.PromoteInput(ctx, input.ID, "task")
	require.NoError(t, err)

	toolCalls := json.RawMessage(`[{"ID":"partial","Name":"bash","Arguments":"eyJjb21tYW5kIjoiZWNobyBiYWQifQ=="}]`)
	reasoning := json.RawMessage(`{"model":"model","payload":[{"type":"reasoning","id":"sealed"}]}`)
	usage := json.RawMessage(`{"promptTokens":11,"completionTokens":13}`)
	result, err := store.CommitRejectedResponse(ctx, RejectedResponse{
		SessionID: root.ID, RootID: root.ID, Iteration: 1,
		Message: &transcript.Message{
			Role: "assistant", Content: "partial secret", ToolCalls: toolCalls,
			ReasoningContent: "unfinished", ReasoningRaw: reasoning,
			CostUSD: 0.25, Usage: usage, FinishType: "length",
			ProviderFinishReason: "max_output_tokens", RejectedReason: "output_length",
		},
	})
	require.NoError(t, err)
	assert.Equal(t, RejectedResponseRecoveryQueued, result.Outcome)
	assert.Positive(t, result.MessageID)
	assert.Positive(t, result.RecoveryMessageID)

	active, err := store.LoadActiveMessages(ctx, root.ID)
	require.NoError(t, err)
	require.Len(t, active, 2)
	assert.Equal(t, "task", active[0].Content)
	assert.Equal(t, OutputLengthRecoveryPrompt, active[1].Content)
	assert.Equal(t, result.MessageID, active[1].RetryOfMessageID)
	assertRejectedAttemptEvidence(t, db, store, root.ID, result, toolCalls, reasoning, usage)
}

func assertRejectedAttemptEvidence(
	t *testing.T,
	db *sql.DB,
	store Store,
	rootID int64,
	result *RejectedResponseResult,
	toolCalls, reasoning, usage json.RawMessage,
) {
	t.Helper()
	ctx := context.Background()

	var got transcript.Message
	var rawCalls, rawReasoning, rawUsage string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT content, tool_calls, reasoning_content, reasoning_raw,
		cost_usd, usage, finish_type, provider_finish_reason, rejected_reason
		FROM messages WHERE id = ?`, result.MessageID).Scan(
		&got.Content, &rawCalls, &got.ReasoningContent, &rawReasoning, &got.CostUSD, &rawUsage,
		&got.FinishType, &got.ProviderFinishReason, &got.RejectedReason,
	))
	assert.Equal(t, "partial secret", got.Content)
	assert.JSONEq(t, string(toolCalls), rawCalls)
	assert.Equal(t, "unfinished", got.ReasoningContent)
	assert.JSONEq(t, string(reasoning), rawReasoning)
	assert.InDelta(t, 0.25, got.CostUSD, 0.000001)
	assert.JSONEq(t, string(usage), rawUsage)
	assert.Equal(t, "length", got.FinishType)
	assert.Equal(t, "max_output_tokens", got.ProviderFinishReason)
	assert.Equal(t, "output_length", got.RejectedReason)

	var outboxRows int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_outbox
		WHERE session_id = ?`, rootID).Scan(&outboxRows))
	assert.Zero(t, outboxRows)

	prompt, completion, cost, err := store.GetSessionTreeUsage(ctx, rootID)
	require.NoError(t, err)
	assert.Equal(t, 11, prompt)
	assert.Equal(t, 13, completion)
	assert.InDelta(t, 0.25, cost, 0.000001)

	facts, err := store.CaptureProgress(ctx, rootID)
	require.NoError(t, err)
	assert.Equal(t, result.RecoveryMessageID, facts.MessageWatermark)
	assert.Equal(t, 11, facts.PromptTokens)
	assert.Equal(t, 13, facts.CompletionTokens)
	assert.Empty(t, facts.LatestModelProgress)
}

func TestResponseIntegrityStoreRepeatedLengthCommitsTerminalError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	root, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "test"})
	require.NoError(t, err)

	first, err := store.CommitRejectedResponse(ctx, rejectedLength(root.ID, 1, 0.1))
	require.NoError(t, err)
	require.Equal(t, RejectedResponseRecoveryQueued, first.Outcome)
	_, err = store.LoadCurrentTerminalRejection(ctx, root.ID)
	require.ErrorIs(t, err, sql.ErrNoRows)
	store = NewStore(db)

	second, err := store.CommitRejectedResponse(ctx, rejectedLength(root.ID, 2, 0.2))
	require.NoError(t, err)
	assert.Equal(t, RejectedResponseRetryExhausted, second.Outcome)
	assert.Positive(t, second.Output.OutputID)
	assert.Zero(t, second.RecoveryMessageID)

	reloaded, err := store.GetSession(ctx, root.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, reloaded.Iteration)
	assert.Equal(t, SessionStatusError, reloaded.Status)

	var content string
	var releases bool
	require.NoError(t, db.QueryRowContext(ctx, `SELECT content, releases_input FROM session_outbox
		WHERE id = ?`, second.Output.OutputID).Scan(&content, &releases))
	assert.Equal(t, IntegrityErrorNotice(OutputLengthTerminalError), content)
	assert.True(t, releases)

	terminal, err := store.LoadCurrentTerminalRejection(ctx, root.ID)
	require.NoError(t, err)
	assert.Equal(t, second.MessageID, terminal.ID)
	assert.Equal(t, "output_length", terminal.RejectedReason)
}

func TestResponseIntegrityStoreAcceptedInputSupersedesRecovery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	root, err := store.CreateSession(ctx, projectID, "model", "", nil)
	require.NoError(t, err)

	first, err := store.CommitRejectedResponse(ctx, rejectedLength(root.ID, 1, 0))
	require.NoError(t, err)
	require.Equal(t, RejectedResponseRecoveryQueued, first.Outcome)

	now := time.Now().UTC()
	message, err := db.ExecContext(ctx, `INSERT INTO messages (session_id, role, content, created_at)
		VALUES (?, 'user', 'real input', ?)`, root.ID, now)
	require.NoError(t, err)
	acceptedID, err := message.LastInsertId()
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO session_inbox
		(session_id, source, raw_content, received_at, state, resolved_at, accepted_message_id)
		VALUES (?, 'user', 'real input', ?, 'accepted', ?, ?)`, root.ID, now, now, acceptedID)
	require.NoError(t, err)

	next, err := store.CommitRejectedResponse(ctx, rejectedLength(root.ID, 2, 0))
	require.NoError(t, err)
	assert.Equal(t, RejectedResponseRecoveryQueued, next.Outcome)
	assert.Positive(t, next.RecoveryMessageID)
}

func TestResponseIntegrityStoreBudgetCrossingSuppressesRecovery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	root, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "test"})
	require.NoError(t, err)
	input, err := store.EnqueueInput(ctx, root.ID, InputSourceUser, "/budget")
	require.NoError(t, err)
	_, _, err = store.PromoteInputWithActivation(ctx, input.ID, "/budget\n\nactivate",
		ActivationDraft{ToolID: "set_budget", Command: "/budget"})
	require.NoError(t, err)
	limit := 0.5
	_, _, err = store.ArmBudget(ctx, BudgetMutation{
		RootSessionID: root.ID, InputID: input.ID, ToolID: "set_budget", Command: "/budget",
		ToolCallID: "arm", CostLimitUSD: &limit, Receipt: "Budget armed",
	})
	require.NoError(t, err)

	rejection := rejectedLength(root.ID, 1, 0.75)
	rejection.Message.ToolCalls = json.RawMessage(`[{"ID":"danger","Name":"bash","Arguments":"e30="}]`)
	result, err := store.CommitRejectedResponse(ctx, rejection)
	require.NoError(t, err)
	assert.Equal(t, RejectedResponseBudgetSuppressed, result.Outcome)
	require.NotNil(t, result.Budget)
	assert.Equal(t, BudgetFired, result.Budget.State)
	assert.Positive(t, result.Output.OutputID)
	assert.Zero(t, result.RecoveryMessageID)

	var toolResults, checkpoints int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages
		WHERE session_id = ? AND role = 'tool' AND tool_call_id = 'danger'`, root.ID).Scan(&toolResults))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_outbox
		WHERE session_id = ? AND source_key = 'budget:1:checkpoint'`, root.ID).Scan(&checkpoints))
	assert.Zero(t, toolResults)
	assert.Equal(t, 1, checkpoints)
}

func TestResponseIntegrityStoreUnknownTerminatesWithoutRecovery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, _, projectID := newTestStore(t)
	root, err := store.CreateSession(ctx, projectID, "model", "", nil)
	require.NoError(t, err)

	result, err := store.CommitRejectedResponse(ctx, RejectedResponse{
		SessionID: root.ID, RootID: root.ID, Iteration: 1,
		Message: &transcript.Message{
			Role: "assistant", Content: "filtered", FinishType: "unknown",
			ProviderFinishReason: "content_filter", RejectedReason: "unknown_finish",
		},
	})
	require.NoError(t, err)
	assert.Equal(t, RejectedResponseUnknownTerminal, result.Outcome)
	assert.Zero(t, result.RecoveryMessageID)

	terminal, err := store.LoadCurrentTerminalRejection(ctx, root.ID)
	require.NoError(t, err)
	assert.Equal(t, "content_filter", terminal.ProviderFinishReason)
}

func TestResponseIntegrityStoreRejectsInvalidRetryReference(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	root, err := store.CreateSession(ctx, projectID, "model", "", nil)
	require.NoError(t, err)

	ordinaryID, err := store.InsertMessage(ctx, root.ID, &transcript.Message{Role: "assistant", Content: "ok"})
	require.NoError(t, err)
	var providerReasonIsNull bool
	require.NoError(t, db.QueryRowContext(ctx, `SELECT provider_finish_reason IS NULL
		FROM messages WHERE id = ?`, ordinaryID).Scan(&providerReasonIsNull))
	assert.True(t, providerReasonIsNull)

	_, err = store.InsertMessage(ctx, root.ID, &transcript.Message{
		Role: "user", Content: OutputLengthRecoveryPrompt, RetryOfMessageID: ordinaryID,
	})
	require.Error(t, err)
	assert.NotErrorIs(t, err, sql.ErrNoRows)
}

func rejectedLength(sessionID int64, iteration int, cost float64) RejectedResponse {
	return RejectedResponse{
		SessionID: sessionID, RootID: sessionID, Iteration: iteration,
		Message: &transcript.Message{
			Role: "assistant", Content: "partial", CostUSD: cost,
			FinishType: "length", ProviderFinishReason: "length", RejectedReason: "output_length",
		},
	}
}
