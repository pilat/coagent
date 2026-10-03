package daemon

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

// An unreadable ledger must not let a child bypass its parent's quota.
func TestEnsureRunner_ClassifyErrorBlocksStart(t *testing.T) {
	var flaky *flakyLinkStore

	h := newSubagentHarnessDecorated(t, trivialRespond, func(inner subagent.Store) subagent.Store {
		flaky = newFlakyLinkStore(inner)
		return flaky
	})
	defer h.shutdown()

	rec, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)

	totalBefore, childrenBefore := runnerRunningCount(h.mgr.runners), runnerChildCount(h.mgr.runners)

	flaky.failGetLink(1, 0)

	err = h.mgr.start(h.ctx, rec.ID)
	require.ErrorIs(t, err, errLinkRead)

	assert.False(t, h.mgr.HasActiveLoop(rec.ID), "no runner for an unclassifiable session")
	assert.Equal(t, totalBefore, runnerRunningCount(h.mgr.runners), "no slot was taken")
	assert.Equal(t, childrenBefore, runnerChildCount(h.mgr.runners))
}

// Failed starts retain their input and wait for the bounded admission retry.
func TestDrainQueue_StartErrorRetainsWaitingInput(t *testing.T) {
	var flaky *flakyLinkStore

	h := newSubagentHarnessDecorated(t, trivialRespond, func(inner subagent.Store) subagent.Store {
		flaky = newFlakyLinkStore(inner)
		return flaky
	})
	defer h.shutdown()

	parent, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)

	childID, err := func() (int64, error) {
		var id int64
		err := h.sessStore.WithTx(h.ctx, func(tx *sql.Tx) error {
			var err error
			id, err = sessionstore.CreateSubagentSessionTx(
				h.ctx,
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
	require.NoError(t, seedChildLink(h.ctx, h.sessStore, subagent.Link{
		ParentID: parent.ID, ChildID: childID, TaskCallID: "bg",
	}))

	h.mgr.runners.wait(waitingRunner{sessionID: childID, parentID: parent.ID, child: true})
	flaky.failGetLink(1, childID)

	core, logs := observer.New(zap.ErrorLevel)
	ctx := logger.ToContext(h.ctx, zap.New(core))

	h.mgr.drain(ctx)

	assert.Equal(t, 1, h.queueLen(), "the failed start retains its waiting entry")
	assert.False(t, h.mgr.HasActiveLoop(childID), "no runner was created")
	assert.NotEmpty(t, logs.FilterMessage("waiting_runner_start_failed").All(), "the failure is logged")

	// A persistent read failure remains queued for the bounded admission retry.
	h.mgr.drain(ctx)
	assert.Equal(t, 1, h.queueLen())
}

// Capacity is rechecked against the durable parent after selecting a waiter.
func TestDrainQueue_CapacityReparks(t *testing.T) {
	h := newSubagentHarnessWith(t, trivialRespond)
	defer h.shutdown()

	parent, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)

	childID, err := func() (int64, error) {
		var id int64
		err := h.sessStore.WithTx(h.ctx, func(tx *sql.Tx) error {
			var err error
			id, err = sessionstore.CreateSubagentSessionTx(
				h.ctx,
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
	// Blocking: a blocking child errors on admit-fail instead of self-queueing,
	// which is the only way to reach the re-park branch.
	require.NoError(t, seedChildLink(h.ctx, h.sessStore, subagent.Link{
		ParentID: parent.ID, ChildID: childID, TaskCallID: "b", Blocking: true,
	}))

	// Selection trusts the parked entry's parent id while start re-derives it
	// from the link; parking under an idle id makes the two disagree on demand.
	const idleParentID = int64(9999)

	h.mgr.runners.wait(waitingRunner{sessionID: childID, parentID: idleParentID, child: true})

	for range maxPerParent {
		require.True(t, h.mgr.runners.tryAdmit(true, parent.ID))
	}

	require.True(t, h.mgr.runners.canAdmit(true, idleParentID), "the peek must let this entry through")

	h.mgr.drain(h.ctx)

	assert.Equal(t, 1, h.queueLen(), "a capacity miss parks the child again")
	assert.False(t, h.mgr.HasActiveLoop(childID), "and does not start it")

	for range maxPerParent {
		h.mgr.runners.release(true, parent.ID)
	}
}
