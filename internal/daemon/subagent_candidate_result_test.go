package daemon

import (
	"database/sql"
	"encoding/json"
	"testing"

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

	iteration := 1
	_, err := h.sessStore.Commit(
		ctx,
		sessionstore.Commit{
			SessionID: childID,
			Messages: []*transcript.Message{
				stored("assistant", "the full child answer"),
				stored("user", "[AUTOMATED CHECK] second look"),
			},
			State: sessionstore.StatePatch{
				Iteration: &iteration,
				Candidate: &sessionstore.CandidateChange{NextRef: 0},
			},
		},
	)
	require.NoError(t, err)

	state, err := h.sessStore.LoadCompletionCheckState(ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, state.CandidateID)
	candidateID := *state.CandidateID

	iteration = 2
	_, err = h.sessStore.Commit(
		ctx,
		sessionstore.Commit{
			SessionID: childID,
			Messages:  []*transcript.Message{stored("assistant", "why I am stopping")},
			State: sessionstore.StatePatch{
				Iteration:         &iteration,
				ConfirmedAnswerID: &candidateID,
				Candidate:         &sessionstore.CandidateChange{Expected: candidateID, NextRef: -1},
			},
		},
	)
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
	childID, err := func() (int64, error) {
		var id int64
		err := h.sessStore.WithTx(ctx, func(tx *sql.Tx) error {
			var err error
			id, err = sessionstore.CreateSubagentSessionTx(
				ctx,
				tx,
				sessionstore.CreateSubagentSession{
					ProjectID:      h.projectID,
					ParentID:       parent.ID,
					RootID:         parent.ID,
					AgentType:      "general",
					Model:          "fake-model",
					ReasoningLevel: "",
				},
			)
			return err
		})
		return id, err
	}()
	require.NoError(t, err)
	require.NoError(t, h.links.InsertSubagentLink(ctx, subagent.Link{
		ParentID: parent.ID, ChildID: childID, TaskCallID: "cand",
	}))

	seedChildCandidateConfirm(t, h, childID)

	h.startInboxWake()

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
	childID, err := func() (int64, error) {
		var id int64
		err := h.sessStore.WithTx(ctx, func(tx *sql.Tx) error {
			var err error
			id, err = sessionstore.CreateSubagentSessionTx(
				ctx,
				tx,
				sessionstore.CreateSubagentSession{
					ProjectID:      h.projectID,
					ParentID:       parent.ID,
					RootID:         parent.ID,
					AgentType:      "general",
					Model:          "fake-model",
					ReasoningLevel: "",
				},
			)
			return err
		})
		return id, err
	}()
	require.NoError(t, err)
	require.NoError(t, h.links.InsertSubagentLink(ctx, subagent.Link{
		ParentID: parent.ID, ChildID: childID, TaskCallID: "stale",
	}))

	seedChildCandidateConfirm(t, h, childID)

	// The child then errors (max iterations persists error status).
	require.NoError(t, h.sessStore.UpdateSessionStatus(ctx, childID, sessionstore.SessionStatusError))
	_, err = h.sessStore.Commit(ctx, sessionstore.Commit{SessionID: childID, Messages: []*transcript.Message{{
		Role: "assistant", ToolCalls: json.RawMessage(`[{"id":"x","name":"bash","arguments":{}}]`),
	}}})
	require.NoError(t, err)

	h.startInboxWake()

	h.mgr.finalizeChild(ctx, childID)

	link, err := h.links.GetLink(ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.Equal(t, subagent.OutcomeIncomplete, link.Outcome,
		"the pointer must not mask a failed child: error precedence gives the incomplete outcome")
	assert.Contains(t, link.Result, "without a final answer", "the failure result stands")
	assert.NotContains(t, link.Result, "the full child answer")
}
