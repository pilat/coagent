package session

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/transcript"
)

// wakeDelegatingBoundary projects the exact session's wake source through the
// real store, so seeded ledger rows (not scripted flags) drive the decision.
type wakeDelegatingBoundary struct {
	loopInputBoundary
	store     sessionstore.Store
	sessionID int64
}

func (b *wakeDelegatingBoundary) HasBackgroundWakeSource(ctx context.Context) (bool, error) {
	return b.store.HasBackgroundWakeSource(ctx, b.sessionID)
}

func newDispositionLoop(t *testing.T) (*svc, *sql.DB, sessionstore.Store, int64, *loopRunner) {
	t.Helper()

	db, store, sessionID := newFinalOutputStore(t)
	agent := newTestAgent()
	agent.store = store
	agent.dispositions = store
	agent.outputStore = store
	agent.id = sessionID
	agent.rootID = sessionID
	agent.outputEnabled = true
	agent.todoStore = todo.New()
	agent.ms = newMessageStore(store, sessionID, store)

	boundary := &wakeDelegatingBoundary{store: store, sessionID: sessionID}
	boundary.agent = agent
	agent.boundary = boundary

	runner := &loopRunner{
		agent: agent, result: &loopResult{}, log: zap.NewNop(),
		replyToInput: true, directReplyEligible: true,
	}

	return agent, db, store, sessionID, runner
}

func outboxRows(t *testing.T, db *sql.DB, sessionID int64) []map[string]any {
	t.Helper()

	rows, err := db.QueryContext(context.Background(),
		`SELECT type, content, releases_input, source_key FROM session_outbox WHERE session_id = ? ORDER BY id`,
		sessionID)
	require.NoError(t, err)
	defer rows.Close()

	var out []map[string]any
	for rows.Next() {
		var typ, content, sourceKey string
		var releases bool
		require.NoError(t, rows.Scan(&typ, &content, &releases, &sourceKey))
		out = append(out, map[string]any{
			"type": typ, "content": content, "releases": releases, "source_key": sourceKey,
		})
	}
	require.NoError(t, rows.Err())

	return out
}

func transcriptRoles(t *testing.T, store sessionstore.Store, sessionID int64) []*transcript.Message {
	t.Helper()

	messages, err := store.LoadActiveMessages(context.Background(), sessionID)
	require.NoError(t, err)

	return messages
}

// A no-wake non-empty stop hides its text: no outbox row, one assistant row
// plus the host nudge, and a pending durable check with a zero empty streak.
func TestCompletionDisposition_CandidateStaysHidden(t *testing.T) {
	_, db, store, sessionID, runner := newDispositionLoop(t)
	ctx := context.Background()

	runner.lastResp = textResponse("first answer")
	require.NoError(t, runner.recordIteration(ctx))

	assert.Empty(t, outboxRows(t, db, sessionID), "an unconfirmed candidate publishes nothing")

	messages := transcriptRoles(t, store, sessionID)
	require.Len(t, messages, 2, "candidate plus its host nudge")
	assert.Equal(t, "assistant", messages[0].Role)
	assert.Equal(t, "first answer", messages[0].Content)
	assert.Equal(t, "user", messages[1].Role)

	state, err := store.LoadCompletionCheckState(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, state.CandidateID)
	assert.Equal(t, messages[0].ID, *state.CandidateID,
		"the durable check points at the hidden candidate row")
	assert.Zero(t, state.EmptyStopStreak)
	assert.Empty(t, runner.result.FinalResponse, "no confirmation, no final response")
}

// The second non-empty stop publishes exactly one persistent releasing output
// carrying the *candidate's* text — the considered answer — while the
// confirming stop's own terse ack is discarded. The check clears and
// FinalResponse settles on the candidate.
func TestCompletionDisposition_ConfirmationPublishesOnce(t *testing.T) {
	_, db, store, sessionID, runner := newDispositionLoop(t)
	ctx := context.Background()

	// A manager-owned turn: the accepted input opens the durable reply
	// obligation, which re-seeds the reply cache on every later turn.
	runner.acceptedManagerInput = true
	runner.lastResp = textResponse("first answer")
	require.NoError(t, runner.recordIteration(ctx))

	runner.lastResp = textResponse("why I am stopping")
	require.NoError(t, runner.recordIteration(ctx))

	rows := outboxRows(t, db, sessionID)
	require.Len(t, rows, 1, "confirmation publishes exactly one output")
	assert.Equal(t, string(sessionstore.OutputMessagePersistent), rows[0]["type"])
	assert.True(t, rows[0]["releases"].(bool), "the confirmed output releases the manager input")
	assert.Contains(t, rows[0]["content"], "first answer",
		"the candidate's full answer is what reaches the manager")
	assert.NotContains(t, rows[0]["content"], "why I am stopping",
		"the nudge ack is discarded, never published")

	messages := transcriptRoles(t, store, sessionID)
	confirmedID := messages[len(messages)-1].ID
	assert.Contains(t, rows[0]["source_key"], fmt.Sprintf("message:%d:final", confirmedID),
		"the final's idempotency key stays keyed to the confirming row")

	state, err := store.LoadCompletionCheckState(ctx, sessionID)
	require.NoError(t, err)
	assert.Nil(t, state.CandidateID, "confirmation clears the pending check")
	assert.Equal(t, "first answer", runner.result.FinalResponse)
}

// A durable wake source owns the next turn: the stop publishes ordinary
// output immediately with no candidate, no nudge, and no second look.
func TestCompletionDisposition_WakeYieldsImmediately(t *testing.T) {
	_, db, store, sessionID, runner := newDispositionLoop(t)
	ctx := context.Background()

	_, err := store.EnqueueAsyncInput(ctx, sessionID, sessionstore.InputSourceProcess, "process done", nil)
	require.NoError(t, err)

	runner.lastResp = textResponse("yielding to background work")
	require.NoError(t, runner.recordIteration(ctx))

	rows := outboxRows(t, db, sessionID)
	require.Len(t, rows, 1, "a wake yield publishes ordinary output immediately")

	state, err := store.LoadCompletionCheckState(ctx, sessionID)
	require.NoError(t, err)
	assert.Nil(t, state.CandidateID, "a wake yield creates no pending check")

	messages := transcriptRoles(t, store, sessionID)
	require.Len(t, messages, 1, "no host nudge accompanies a wake yield")
	assert.Equal(t, "assistant", messages[0].Role)
}

// Empty no-wake stops count toward the durable streak without publishing;
// the pending candidate survives them and a later non-empty stop confirms it.
func TestCompletionDisposition_EmptyStreakThenConfirm(t *testing.T) {
	_, db, store, sessionID, runner := newDispositionLoop(t)
	ctx := context.Background()

	runner.lastResp = textResponse("first answer")
	require.NoError(t, runner.recordIteration(ctx))

	for range 2 {
		runner.lastResp = &llmwire.Response{FinishType: llmwire.FinishStop}
		require.NoError(t, runner.recordIteration(ctx))
	}

	state, err := store.LoadCompletionCheckState(ctx, sessionID)
	require.NoError(t, err)
	assert.Equal(t, 2, state.EmptyStopStreak)
	assert.Empty(t, outboxRows(t, db, sessionID), "empty stops publish nothing")

	messages := transcriptRoles(t, store, sessionID)
	var nudges int
	for _, message := range messages {
		if message.Role == "user" {
			nudges++
		}
	}
	assert.Equal(t, 3, nudges, "candidate nudge plus two empty-stop nudges")

	runner.lastResp = textResponse("confirmed after empties")
	require.NoError(t, runner.recordIteration(ctx))

	rows := outboxRows(t, db, sessionID)
	require.Len(t, rows, 1, "the later non-empty stop confirms the surviving candidate")

	state, err = store.LoadCompletionCheckState(ctx, sessionID)
	require.NoError(t, err)
	assert.Nil(t, state.CandidateID)
	assert.Zero(t, state.EmptyStopStreak, "confirmation resets the empty streak")
	assert.Equal(t, "first answer", runner.result.FinalResponse,
		"the surviving candidate, not the confirming stop's ack, is the final")
}

// A budget-fired confirming stop suppresses the outbox row inside the same
// transaction that already cleared the candidate: the answer is dropped, not
// deferred. Accepted ack-loss-on-budget semantics, locked explicitly so no one
// "fixes" it into a partial publish.
func TestCompletionDisposition_BudgetFiredConfirmDropsTheAnswer(t *testing.T) {
	_, db, store, sessionID, runner := newDispositionLoop(t)
	ctx := context.Background()

	runner.lastResp = textResponse("first answer")
	require.NoError(t, runner.recordIteration(ctx))

	// The limit sits below the confirming attempt's cost, so the disposition
	// transaction itself observes the crossing and suppresses the final.
	_, err := db.ExecContext(ctx, `INSERT INTO session_budgets
		(root_session_id, state, generation, armed_at, baseline_cost_usd, cost_limit_usd)
		VALUES (?, 'armed', 1, datetime('now'), 0, 0.000001)`, sessionID)
	require.NoError(t, err)

	runner.lastResp = &llmwire.Response{
		Text: "why I am stopping", FinishType: llmwire.FinishStop, CostUSD: 0.01,
	}
	require.NoError(t, runner.recordIteration(ctx))

	rows := outboxRows(t, db, sessionID)
	require.Len(t, rows, 1, "only the host budget checkpoint commits")
	assert.Contains(t, rows[0]["content"], "Budget checkpoint reached")
	assert.NotContains(t, rows[0]["content"], "first answer",
		"the candidate was cleared before the suppressed commit: dropped, not deferred")
	assert.NotContains(t, rows[0]["content"], "why I am stopping")

	state, err := store.LoadCompletionCheckState(ctx, sessionID)
	require.NoError(t, err)
	assert.Nil(t, state.CandidateID,
		"the candidate was cleared before the suppressed commit and must not resurface")
}

// A child follows the same two-phase script through the disposition path: the
// first no-wake stop stays hidden, and the second confirms through the
// transcript alone — no outbox row for an output-disabled child.
func TestCompletionDisposition_ChildParity(t *testing.T) {
	_, db, store, parentID, _ := newDispositionLoop(t)
	ctx := context.Background()

	childID := func() int64 {
		var projectID int64
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT project_id FROM sessions WHERE id = ?`, parentID).Scan(&projectID))

		id, err := store.CreateSubagentSession(ctx, projectID, parentID, parentID, "general", "model", "")
		require.NoError(t, err)

		return id
	}()

	_, err := db.ExecContext(ctx, `INSERT INTO subagent_links
		(parent_id, child_id, task_call_id, blocking, depth, state, created_at)
		VALUES (?, ?, ?, 0, 0, 'running', ?)`,
		parentID, childID, "task-1", time.Now().UTC().Unix())
	require.NoError(t, err)

	agent := newTestAgent()
	agent.store = store
	agent.dispositions = store
	agent.outputStore = store
	agent.id = childID
	agent.rootID = parentID
	agent.outputEnabled = false
	agent.todoStore = todo.New()
	agent.ms = newMessageStore(store, childID, store)

	boundary := &wakeDelegatingBoundary{store: store, sessionID: childID}
	boundary.agent = agent
	agent.boundary = boundary

	runner := &loopRunner{
		agent: agent, result: &loopResult{}, log: zap.NewNop(),
		replyToInput: true, directReplyEligible: true,
	}

	runner.lastResp = textResponse("child first answer")
	require.NoError(t, runner.recordIteration(ctx))

	assert.Empty(t, outboxRows(t, db, childID), "an unconfirmed candidate publishes nothing")

	messages := transcriptRoles(t, store, childID)
	require.Len(t, messages, 2, "candidate plus its host nudge")

	state, err := store.LoadCompletionCheckState(ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, state.CandidateID)
	assert.Equal(t, messages[0].ID, *state.CandidateID)

	runner.lastResp = textResponse("child confirmed answer")
	require.NoError(t, runner.recordIteration(ctx))

	assert.Empty(t, outboxRows(t, db, childID),
		"an output-disabled child confirms through the transcript, not the outbox")

	state, err = store.LoadCompletionCheckState(ctx, childID)
	require.NoError(t, err)
	assert.Nil(t, state.CandidateID, "confirmation clears the pending check")
	assert.Equal(t, "child first answer", runner.result.FinalResponse,
		"the child's recovery value is the candidate text, not the ack")
}

// An empty stop with a durable wake source yields at once: the streak never
// moves, no nudge is appended, and no check is opened.
func TestCompletionDisposition_EmptyWakeYieldsImmediately(t *testing.T) {
	_, db, store, sessionID, runner := newDispositionLoop(t)
	ctx := context.Background()

	_, err := store.EnqueueAsyncInput(ctx, sessionID, sessionstore.InputSourceProcess, "process done", nil)
	require.NoError(t, err)

	runner.lastResp = &llmwire.Response{FinishType: llmwire.FinishStop}
	require.NoError(t, runner.recordIteration(ctx))

	state, err := store.LoadCompletionCheckState(ctx, sessionID)
	require.NoError(t, err)
	assert.Zero(t, state.EmptyStopStreak, "a wake yield is not an empty attempt")
	assert.Nil(t, state.CandidateID, "a wake yield opens no pending check")

	assert.Empty(t, outboxRows(t, db, sessionID), "an empty wake yield publishes nothing")

	messages := transcriptRoles(t, store, sessionID)
	require.Len(t, messages, 1, "only the empty assistant evidence row is stored")
	assert.Equal(t, "assistant", messages[0].Role)
	assert.Empty(t, messages[0].Content)
	for _, message := range messages {
		assert.NotEqual(t, "user", message.Role, "an empty wake yield appends no nudge")
	}
}

// A tool_calls finish with no calls follows empty-response recovery: its body,
// even when text rides along, is never a final answer, so the durable streak
// counts the attempt and the terminal notice commits at the sixth.
func TestCompletionDisposition_ToolCallsFinishWithoutCallsRecoversAsEmpty(t *testing.T) {
	_, db, store, sessionID, runner := newDispositionLoop(t)
	ctx := context.Background()

	runner.lastResp = &llmwire.Response{Text: "stop with a tool_calls label", FinishType: llmwire.FinishToolCalls}
	require.NoError(t, runner.recordIteration(ctx))

	assert.Empty(t, outboxRows(t, db, sessionID), "the structurally empty body publishes nothing")

	state, err := store.LoadCompletionCheckState(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, 1, state.EmptyStopStreak, "the rejected body counts as one empty attempt")
	assert.Nil(t, state.CandidateID, "a rejected body never opens a pending check")

	messages := transcriptRoles(t, store, sessionID)
	require.Len(t, messages, 2, "the attempt plus its continuation nudge")
	assert.Equal(t, "assistant", messages[0].Role)
	assert.Equal(t, "stop with a tool_calls label", messages[0].Content)
	assert.Equal(t, "user", messages[1].Role)
	assert.NotContains(t, messages[1].Content, "stop with a tool_calls label")

	for range sessionstore.EmptyStopTerminalStreak - 1 {
		runner.lastResp = &llmwire.Response{Text: "hidden on every attempt", FinishType: llmwire.FinishToolCalls}
		require.NoError(t, runner.recordIteration(ctx))
	}

	rows := outboxRows(t, db, sessionID)
	require.Len(t, rows, 1, "the terminal empty-stop notice commits once")
	assert.Contains(t, rows[0]["content"], sessionstore.EmptyStopTerminalNotice(sessionstore.EmptyStopTerminalStreak))
	assert.NotContains(t, rows[0]["content"], "stop with a tool_calls label")
	assert.NotContains(t, rows[0]["content"], "hidden on every attempt")
}

// An empty wake yield preserves the durable streak instead of resetting it:
// the yield neither advances nor clears the count. A non-empty yield resets
// it like any other non-empty accepted response.
func TestCompletionDisposition_EmptyWakePreservesStreak(t *testing.T) {
	_, db, store, sessionID, runner := newDispositionLoop(t)
	ctx := context.Background()

	runner.lastResp = &llmwire.Response{FinishType: llmwire.FinishStop}
	require.NoError(t, runner.recordIteration(ctx))
	runner.lastResp = &llmwire.Response{FinishType: llmwire.FinishStop}
	require.NoError(t, runner.recordIteration(ctx))

	state, err := store.LoadCompletionCheckState(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, 2, state.EmptyStopStreak)

	_, err = store.EnqueueAsyncInput(ctx, sessionID, sessionstore.InputSourceProcess, "process done", nil)
	require.NoError(t, err)

	runner.lastResp = &llmwire.Response{FinishType: llmwire.FinishStop}
	require.NoError(t, runner.recordIteration(ctx))

	state, err = store.LoadCompletionCheckState(ctx, sessionID)
	require.NoError(t, err)
	assert.Equal(t, 2, state.EmptyStopStreak, "an empty wake yield preserves the prior streak")
	assert.Empty(t, outboxRows(t, db, sessionID), "empty stops publish nothing")

	runner.lastResp = textResponse("yielding to the running process")
	require.NoError(t, runner.recordIteration(ctx))

	state, err = store.LoadCompletionCheckState(ctx, sessionID)
	require.NoError(t, err)
	assert.Zero(t, state.EmptyStopStreak, "a non-empty wake yield resets the streak")
}

// Fresh manager input invalidates the pending check in its own commit: the
// next non-empty stop opens a new candidate instead of confirming the old one.
func TestCompletionDisposition_ManagerInputWhilePendingStartsNewCheck(t *testing.T) {
	_, db, store, sessionID, runner := newDispositionLoop(t)
	ctx := context.Background()

	runner.lastResp = textResponse("first answer")
	require.NoError(t, runner.recordIteration(ctx))

	state, err := store.LoadCompletionCheckState(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, state.CandidateID, "the first stop opens a pending check")

	input, err := store.EnqueueInput(ctx, sessionID, sessionstore.InputSourceUser, "fresh manager input")
	require.NoError(t, err)
	_, err = store.PromoteInput(ctx, input.ID, "fresh manager input")
	require.NoError(t, err)

	state, err = store.LoadCompletionCheckState(ctx, sessionID)
	require.NoError(t, err)
	assert.Nil(t, state.CandidateID, "promotion clears the stale candidate")

	runner.lastResp = textResponse("second answer")
	require.NoError(t, runner.recordIteration(ctx))

	assert.Empty(t, outboxRows(t, db, sessionID),
		"the stop after fresh input is a new candidate, not a confirmation")

	state, err = store.LoadCompletionCheckState(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, state.CandidateID, "the later stop opens a new pending check")
}
