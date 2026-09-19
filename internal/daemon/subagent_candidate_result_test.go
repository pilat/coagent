package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/transcript"
)

// seedChildCandidateConfirm drives the child through the two-phase check at
// the store level: a hidden candidate plus its host nudge, then a confirming
// (ack) stop. It returns the candidate's transcript id.
func seedChildCandidateConfirm(t *testing.T, h *subagentHarness, childID int64) int64 {
	t.Helper()
	ctx := h.ctx

	stored := func(role, content string) *transcript.Message {
		return &transcript.Message{Role: role, Content: content}
	}

	_, err := h.sessStore.CommitAcceptedResponseDisposition(ctx, sessionstore.AcceptedResponseDisposition{
		SessionID: childID, RootID: childID, Iteration: 1,
		Message:    stored("assistant", "the full child answer"),
		Kind:       sessionstore.ResponseDispositionCandidate,
		Nudge:      stored("user", "[AUTOMATED CHECK] second look"),
		ObservedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	state, err := h.sessStore.LoadCompletionCheckState(ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, state.CandidateID)
	candidateID := *state.CandidateID

	_, err = h.sessStore.CommitAcceptedResponseDisposition(ctx, sessionstore.AcceptedResponseDisposition{
		SessionID: childID, RootID: childID, Iteration: 2,
		Message:             stored("assistant", "why I am stopping"),
		Kind:                sessionstore.ResponseDispositionConfirmed,
		ExpectedCandidateID: candidateID,
		ObservedAt:          time.Now().UTC(),
	})
	require.NoError(t, err)

	require.NoError(t, h.sessStore.UpdateSessionStatus(ctx, childID, sessionstore.SessionStatusCompleted))

	var pointer int64
	require.NoError(t, h.db.QueryRowContext(ctx,
		`SELECT COALESCE(completion_check_confirmed_answer_id, 0) FROM sessions WHERE id = ?`,
		childID).Scan(&pointer))
	require.Equal(t, candidateID, pointer, "confirm set the durable answer pointer")

	return candidateID
}

// A child that stops with a full candidate, then confirms with a terse ack,
// hands its parent the candidate text as the result — not the ack. This is the
// real deriveOutcome consumer of the confirmed-answer pointer.
func TestSubagentResult_CarriesCandidateAnswerNotAck(t *testing.T) {
	h := newSubagentHarnessWith(t, trivialRespond)
	defer h.shutdown()

	ctx := h.ctx
	parent, err := h.sessStore.CreateSession(ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)
	childID, err := h.sessStore.CreateSubagentSession(
		ctx, h.projectID, parent.ID, parent.ID, "general", "fake-model", "",
	)
	require.NoError(t, err)
	require.NoError(t, h.links.InsertSubagentLink(ctx, subagent.Link{
		ParentID: parent.ID, ChildID: childID, TaskCallID: "cand",
	}))

	seedChildCandidateConfirm(t, h, childID)

	h.mgr.finalizeChild(ctx, childID)

	link, err := h.links.GetLink(ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.Equal(t, subagent.OutcomeCompleted, link.Outcome)
	assert.Contains(t, link.Result, "the full child answer",
		"the child result is the candidate answer")
	assert.NotContains(t, link.Result, "why I am stopping",
		"the ack never becomes the child result")
}

// A stale pointer never turns an errored child into a completed answer: the
// error outcome keeps precedence over the confirmed-answer pointer.
func TestSubagentResult_ErrorBeatsStalePointer(t *testing.T) {
	h := newSubagentHarnessWith(t, trivialRespond)
	defer h.shutdown()

	ctx := h.ctx
	parent, err := h.sessStore.CreateSession(ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)
	childID, err := h.sessStore.CreateSubagentSession(
		ctx, h.projectID, parent.ID, parent.ID, "general", "fake-model", "",
	)
	require.NoError(t, err)
	require.NoError(t, h.links.InsertSubagentLink(ctx, subagent.Link{
		ParentID: parent.ID, ChildID: childID, TaskCallID: "stale",
	}))

	seedChildCandidateConfirm(t, h, childID)

	// The child then errors (max iterations persists error status).
	require.NoError(t, h.sessStore.UpdateSessionStatus(ctx, childID, sessionstore.SessionStatusError))
	_, err = h.sessStore.InsertMessage(ctx, childID, &transcript.Message{
		Role: "assistant", ToolCalls: json.RawMessage(`[{"id":"x","name":"bash","arguments":{}}]`),
	})
	require.NoError(t, err)

	h.mgr.finalizeChild(ctx, childID)

	link, err := h.links.GetLink(ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.Equal(t, subagent.OutcomeIncomplete, link.Outcome,
		"the pointer must not mask a failed child: error precedence gives the incomplete outcome")
	assert.Contains(t, link.Result, "without a final answer", "the failure result stands")
	assert.NotContains(t, link.Result, "the full child answer")
}
