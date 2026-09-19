package session

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

// A tool-bearing response after a candidate continues the turn: the check
// clears, the candidate publishes nothing of its own, and the eventual real
// stop becomes the final — no frozen bubbles from the abandoned candidate.
func TestCompletionDisposition_ContinueAfterCandidateClearsCheck(t *testing.T) {
	_, db, store, sessionID, runner := newDispositionLoop(t)
	ctx := context.Background()

	runner.lastResp = textResponse("premature candidate")
	require.NoError(t, runner.recordIteration(ctx))

	runner.lastResp = toolResponse()
	require.NoError(t, runner.recordIteration(ctx))

	state, err := store.LoadCompletionCheckState(ctx, sessionID)
	require.NoError(t, err)
	assert.Nil(t, state.CandidateID, "a tool-bearing response clears the pending check")
	assert.Empty(t, outboxRows(t, db, sessionID),
		"the abandoned candidate never publishes; work continues")
	assert.Empty(t, runner.result.FinalResponse, "no final on a continue")

	// The eventual real stop opens a fresh check: only a later confirm is final.
	runner.lastResp = textResponse("the real answer")
	require.NoError(t, runner.recordIteration(ctx))

	state, err = store.LoadCompletionCheckState(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, state.CandidateID, "the real stop opens its own check")
	assert.Empty(t, outboxRows(t, db, sessionID))
}

// The named corner case: the nudge turn continues by spinning up a subagent.
// The tool-bearing stop clears the candidate (Q2) while the running child
// raises the background badge (Q1) on the card it rolls forward to.
func TestCompletionDisposition_ContinueViaSubagentSpinUpClearsAndMarksBackground(t *testing.T) {
	_, db, store, sessionID, runner := newDispositionLoop(t)
	ctx := context.Background()

	runner.lastResp = textResponse("premature candidate")
	require.NoError(t, runner.recordIteration(ctx))

	runner.lastResp = toolResponse()
	require.NoError(t, runner.recordIteration(ctx))

	state, err := store.LoadCompletionCheckState(ctx, sessionID)
	require.NoError(t, err)
	assert.Nil(t, state.CandidateID, "Q2: the continue clears the candidate")

	// A real running child link raises the durable background signal (Q1's
	// inputs); the projection joins the child session row.
	var projectID int64
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT project_id FROM sessions WHERE id = ?`, sessionID).Scan(&projectID))
	childID, err := store.CreateSubagentSession(ctx, projectID, sessionID, sessionID, "general", "m", "")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO subagent_links
		(parent_id, child_id, task_call_id, blocking, depth, state, created_at)
		VALUES (?, ?, ?, 0, 0, 'running', 0)`,
		sessionID, childID, "task-spin")
	require.NoError(t, err)

	facts, err := store.CaptureProgress(ctx, sessionID)
	require.NoError(t, err)
	assert.Equal(t, 1, facts.ActiveSubagents,
		"Q1: the running child feeds the 🟣 Background title")
	assert.Equal(t, 1, facts.BackgroundSubagents, "a non-blocking child is background")
}

// Several premature candidates in a row: none publishes; only the final
// confirmed candidate ever becomes a persistent bubble.
func TestCompletionDisposition_MultipleCandidatesLeaveNoFrozenBubbles(t *testing.T) {
	_, db, _, sessionID, runner := newDispositionLoop(t)
	ctx := context.Background()

	for _, text := range []string{"candidate one", "candidate two"} {
		runner.lastResp = textResponse(text)
		require.NoError(t, runner.recordIteration(ctx))

		runner.lastResp = toolResponse()
		require.NoError(t, runner.recordIteration(ctx))
	}

	assert.Empty(t, outboxRows(t, db, sessionID), "abandoned candidates never freeze as bubbles")

	runner.lastResp = textResponse("the answer that sticks")
	require.NoError(t, runner.recordIteration(ctx))
	runner.lastResp = textResponse("confirming the sticking answer")
	require.NoError(t, runner.recordIteration(ctx))

	rows := outboxRows(t, db, sessionID)
	require.Len(t, rows, 1, "exactly one persistent final")
	assert.Contains(t, rows[0]["content"], "the answer that sticks",
		"the last confirmed candidate is what publishes")
}

func toolResponse() *llmwire.Response {
	return &llmwire.Response{
		Text: "continuing with tools",
		ToolCalls: []llmwire.ToolCall{{
			ID: "call-1", Name: "bash", Arguments: []byte(`{}`),
		}},
	}
}

var _ = sessionstore.OutputMessagePersistent
