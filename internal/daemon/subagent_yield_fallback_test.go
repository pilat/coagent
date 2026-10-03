package daemon

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/transcript"
)

// Activation 1 confirms (pointer set), a re-activation ends in a
// background-yield final: deriveOutcome must return the yield text, never the
// stale confirmed answer — the pointer is cleared by external input, and
// without it the last-assistant-text fallback stands.
func TestSubagentResult_BackgroundYieldFallsBackToYieldText(t *testing.T) {
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
	require.NoError(t, seedChildLink(ctx, h.sessStore, subagent.Link{
		ParentID: parent.ID, ChildID: childID, TaskCallID: "yield",
	}))

	// Activation 1: candidate + confirm leaves the durable pointer.
	seedChildCandidateConfirm(t, h, childID)

	// The re-activation's external model-visible input clears the stale
	// pointer alongside the check — clearing happens at promotion, not at
	// enqueue.
	input, err := h.sessStore.Enqueue(
		ctx,
		sessionstore.Input{
			SessionID:  childID,
			Source:     sessionstore.InputSourceProcess,
			Content:    "wake",
			Attributes: nil,
		},
	)
	require.NoError(t, err)
	_, err = h.sessStore.Commit(
		ctx,
		sessionstore.Commit{
			SessionID: input.Input.SessionID,
			Accept: []sessionstore.Accept{
				{
					InputID:    input.Input.ID,
					State:      sessionstore.InputStateAccepted,
					Content:    "wake",
					LinkRef:    -1,
					ModelBound: true,
				},
			},
		},
	)
	require.NoError(t, err)

	// Activation 2 ends in a plain yield final (no pending check).
	_, err = h.sessStore.Commit(ctx, sessionstore.Commit{SessionID: childID, Messages: []*transcript.Message{{
		Role: "assistant", Content: "fresh yield text",
	}}})
	require.NoError(t, err)
	require.NoError(t, h.sessStore.UpdateSessionStatus(ctx, childID, sessionstore.SessionStatusCompleted))

	var pointer int64
	require.NoError(t, h.db.QueryRowContext(ctx,
		`SELECT COALESCE(completion_check_confirmed_answer_id, 0) FROM sessions WHERE id = ?`,
		childID).Scan(&pointer))
	assert.Zero(t, pointer, "external input clears the stale pointer")

	h.startInboxWake()

	finalizeTestChild(ctx, t, h.mgr, childID)

	link, err := h.links.GetLink(ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.Equal(t, subagent.OutcomeCompleted, link.Outcome)
	assert.Contains(t, link.Result, "fresh yield text", "the fallback is the real final text")
	assert.NotContains(t, link.Result, "the full child answer",
		"the stale confirmed answer must not resurface as the result")
	_ = sessionstore.SessionStatusCompleted
}
