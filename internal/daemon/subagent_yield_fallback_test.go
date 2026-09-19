package daemon

import (
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
	childID, err := h.sessStore.CreateSubagentSession(
		ctx, h.projectID, parent.ID, parent.ID, "general", "fake-model", "",
	)
	require.NoError(t, err)
	require.NoError(t, h.links.InsertSubagentLink(ctx, subagent.Link{
		ParentID: parent.ID, ChildID: childID, TaskCallID: "yield",
	}))

	// Activation 1: candidate + confirm leaves the durable pointer.
	seedChildCandidateConfirm(t, h, childID)

	// The re-activation's external model-visible input clears the stale
	// pointer alongside the check — clearing happens at promotion, not at
	// enqueue.
	input, err := h.sessStore.EnqueueAsyncInput(
		ctx, childID, sessionstore.InputSourceProcess, "wake", nil)
	require.NoError(t, err)
	_, err = h.sessStore.PromoteInput(ctx, input.ID, "wake")
	require.NoError(t, err)

	// Activation 2 ends in a plain yield final (no pending check).
	_, err = h.sessStore.InsertMessage(ctx, childID, &transcript.Message{
		Role: "assistant", Content: "fresh yield text",
	})
	require.NoError(t, err)
	require.NoError(t, h.sessStore.UpdateSessionStatus(ctx, childID, sessionstore.SessionStatusCompleted))

	var pointer int64
	require.NoError(t, h.db.QueryRowContext(ctx,
		`SELECT COALESCE(completion_check_confirmed_answer_id, 0) FROM sessions WHERE id = ?`,
		childID).Scan(&pointer))
	assert.Zero(t, pointer, "external input clears the stale pointer")

	h.mgr.finalizeChild(ctx, childID)

	link, err := h.links.GetLink(ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.Equal(t, subagent.OutcomeCompleted, link.Outcome)
	assert.Contains(t, link.Result, "fresh yield text", "the fallback is the real final text")
	assert.NotContains(t, link.Result, "the full child answer",
		"the stale confirmed answer must not resurface as the result")
	_ = sessionstore.SessionStatusCompleted
}
