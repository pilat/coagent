package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pilat/coagent/internal/admission"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/subagent"
)

// TestEnsureRunner_ClassifyErrorBlocksStart: an unreadable ledger must abort the
// start instead of classifying the session as a root parent — that is what lets
// a child run outside the per-parent quota.
func TestEnsureRunner_ClassifyErrorBlocksStart(t *testing.T) {
	var flaky *flakyLinkStore

	h := newSubagentHarnessDecorated(t, trivialRespond, func(inner subagent.Store) subagent.Store {
		flaky = newFlakyLinkStore(inner)
		return flaky
	})
	defer h.shutdown()

	rec, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)

	totalBefore, childrenBefore := h.mgr.admit.LiveTotal(), h.mgr.admit.LiveChildren()

	flaky.failGetLink(1, 0)

	err = h.mgr.ensureRunner(h.ctx, rec.ID, "/tmp", h.projectID, nil)
	require.ErrorIs(t, err, errLinkRead)

	assert.False(t, h.mgr.HasActiveLoop(rec.ID), "no runner for an unclassifiable session")
	assert.Equal(t, totalBefore, h.mgr.admit.LiveTotal(), "no slot was taken")
	assert.Equal(t, childrenBefore, h.mgr.admit.LiveChildren())
}

// TestDrainQueue_StartErrorDoesNotRepark: a persistent ledger failure is not a
// lost admission race — re-parking on it would spin the queue forever.
func TestDrainQueue_StartErrorDoesNotRepark(t *testing.T) {
	var flaky *flakyLinkStore

	h := newSubagentHarnessDecorated(t, trivialRespond, func(inner subagent.Store) subagent.Store {
		flaky = newFlakyLinkStore(inner)
		return flaky
	})
	defer h.shutdown()

	parent, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)

	childID, err := h.sessStore.CreateSubagentSession(
		h.ctx, h.projectID, parent.ID, parent.ID, "general", "fake-model", "",
	)
	require.NoError(t, err)
	require.NoError(t, h.links.InsertSubagentLink(h.ctx, subagent.Link{
		ParentID: parent.ID, ChildID: childID, TaskCallID: "bg",
	}))

	h.mgr.enqueueChild(h.ctx, childID, parent.ID, "/tmp", h.projectID)
	// Call 1 is drainQueue's own liveness check; the failure lands on call 2,
	// ensureRunner's classification read.
	flaky.failGetLink(2, childID)

	core, logs := observer.New(zap.ErrorLevel)
	ctx := logger.ToContext(h.ctx, zap.New(core))

	h.mgr.drainQueue(ctx)

	assert.Equal(t, 0, h.queueLen(), "the entry is not re-parked on a read failure")
	assert.False(t, h.mgr.HasActiveLoop(childID), "no runner was created")
	assert.NotEmpty(t, logs.FilterMessage("queued_child_start_failed").All(), "the drop is logged loudly")

	// A second drain has nothing left to spin on.
	h.mgr.drainQueue(ctx)
	assert.Equal(t, 0, h.queueLen())
}

// TestDrainQueue_CapacityReparks: the sentinel — and only the sentinel — still
// puts a child back in the queue.
func TestDrainQueue_CapacityReparks(t *testing.T) {
	h := newSubagentHarnessWith(t, trivialRespond)
	defer h.shutdown()

	parent, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)

	childID, err := h.sessStore.CreateSubagentSession(
		h.ctx, h.projectID, parent.ID, parent.ID, "general", "fake-model", "",
	)
	require.NoError(t, err)
	// Blocking: a blocking child errors on admit-fail instead of self-queueing,
	// which is the only way to reach the re-park branch.
	require.NoError(t, h.links.InsertSubagentLink(h.ctx, subagent.Link{
		ParentID: parent.ID, ChildID: childID, TaskCallID: "b", Blocking: true,
	}))

	// The peek trusts the parked entry's parent id while ensureRunner re-derives it
	// from the link; parking under an idle id makes the two disagree on demand.
	const idleParentID = int64(9999)

	h.mgr.enqueueChild(h.ctx, childID, idleParentID, "/tmp", h.projectID)

	for range admission.MaxPerParent {
		require.True(t, h.mgr.admit.TryAdmit(admission.Child, parent.ID))
	}

	require.True(t, h.mgr.admit.CanAdmitChild(idleParentID), "the peek must let this entry through")

	h.mgr.drainQueue(h.ctx)

	assert.Equal(t, 1, h.queueLen(), "a capacity miss parks the child again")
	assert.False(t, h.mgr.HasActiveLoop(childID), "and does not start it")

	for range admission.MaxPerParent {
		h.mgr.admit.Release(admission.Child, parent.ID)
	}
}
