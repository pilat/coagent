package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

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

	totalBefore, childrenBefore := h.mgr.admit.liveTotal(), h.mgr.admit.liveChildren()

	flaky.failGetLink(1, 0)

	err = h.mgr.ensureRunner(h.ctx, rec.ID, "/tmp", h.projectID, nil)
	require.ErrorIs(t, err, errLinkRead)

	assert.False(t, h.mgr.HasActiveLoop(rec.ID), "no runner for an unclassifiable session")
	assert.Equal(t, totalBefore, h.mgr.admit.liveTotal(), "no slot was taken")
	assert.Equal(t, childrenBefore, h.mgr.admit.liveChildren())
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

	for range maxInFlightPerParent {
		require.True(t, h.mgr.admit.tryAdmit(slotChild, parent.ID))
	}

	require.True(t, h.mgr.admit.canAdmitChild(idleParentID), "the peek must let this entry through")

	h.mgr.drainQueue(h.ctx)

	assert.Equal(t, 1, h.queueLen(), "a capacity miss parks the child again")
	assert.False(t, h.mgr.HasActiveLoop(childID), "and does not start it")

	for range maxInFlightPerParent {
		h.mgr.admit.release(slotChild, parent.ID)
	}
}

// TestRunSession_TimeoutReadErrorRunsFullTeardown: aborting on an unresolvable
// timeout must still run the teardown — an earlier bail-out would leave rs.done
// open and hang every stop() on this session forever.
func TestRunSession_TimeoutReadErrorRunsFullTeardown(t *testing.T) {
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
		ParentID: parent.ID, ChildID: childID, TaskCallID: "b", Blocking: true,
	}))

	childrenBefore := h.mgr.admit.liveChildren()
	require.True(t, h.mgr.admit.tryAdmit(slotChild, parent.ID))

	core, logs := observer.New(zap.ErrorLevel)
	loopCtx, loopCancel := context.WithCancel(logger.ToContext(context.Background(), zap.New(core)))

	defer loopCancel()

	rs := &runner{
		cancel:    loopCancel,
		done:      make(chan struct{}),
		workDir:   "/tmp",
		projectID: h.projectID,
		kind:      slotChild,
		parentID:  parent.ID,
	}

	h.mgr.mu.Lock()
	h.mgr.loops[childID] = rs
	h.mgr.mu.Unlock()

	flaky.failGetLink(1, childID)

	go h.mgr.runSession(loopCtx, childID, rs)

	select {
	case <-rs.done:
	case <-time.After(5 * time.Second):
		t.Fatal("runSession never closed rs.done — stop() would hang forever")
	}

	assert.False(t, h.mgr.HasActiveLoop(childID), "the runner is unregistered")
	assert.Equal(t, childrenBefore, h.mgr.admit.liveChildren(), "the child slot is released")
	assert.NotEmpty(t, logs.FilterMessage("child_timeout_unresolved").All())
}

// TestRunSession_TimeoutReadErrorFinalizesAsError: on an intermittent failure the
// ledger recovers before teardown, so finalizeChild gets to write an outcome for a
// child that never ran an iteration — it must not be "completed".
func TestRunSession_TimeoutReadErrorFinalizesAsError(t *testing.T) {
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
		ParentID: parent.ID, ChildID: childID, TaskCallID: "b", Blocking: true,
	}))

	require.True(t, h.mgr.admit.tryAdmit(slotChild, parent.ID))

	loopCtx, loopCancel := context.WithCancel(h.ctx)
	defer loopCancel()

	rs := &runner{
		cancel:    loopCancel,
		done:      make(chan struct{}),
		workDir:   "/tmp",
		projectID: h.projectID,
		kind:      slotChild,
		parentID:  parent.ID,
	}

	h.mgr.mu.Lock()
	h.mgr.loops[childID] = rs
	h.mgr.mu.Unlock()

	// Only applyChildTimeout's read fails; finalizeChild's reads succeed.
	flaky.getLinkFailOnly = 1
	flaky.getLinkFailFor = childID

	go h.mgr.runSession(loopCtx, childID, rs)

	select {
	case <-rs.done:
	case <-time.After(5 * time.Second):
		t.Fatal("runSession did not tear down")
	}

	link, err := h.links.GetLink(h.ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.Equal(t, subagent.StateError, link.State, "a child that never started is not completed")
}

// TestApplyChildTimeout_Deadlines: the normal branches keep their behaviour —
// only blocking children get a deadline, from the link or the default.
func TestApplyChildTimeout_Deadlines(t *testing.T) {
	h := newSubagentHarnessWith(t, trivialRespond)
	defer h.shutdown()

	parent, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)

	newChild := func(callID string, blocking bool, timeoutSec int) int64 {
		id, cerr := h.sessStore.CreateSubagentSession(
			h.ctx, h.projectID, parent.ID, parent.ID, "general", "fake-model", "",
		)
		require.NoError(t, cerr)
		require.NoError(t, h.links.InsertSubagentLink(h.ctx, subagent.Link{
			ParentID: parent.ID, ChildID: id, TaskCallID: callID,
			Blocking: blocking, TimeoutSec: timeoutSec,
		}))

		return id
	}

	tests := []struct {
		name     string
		blocking bool
		timeout  int
		want     time.Duration
	}{
		{name: "explicit timeout", blocking: true, timeout: 60, want: 60 * time.Second},
		{name: "default timeout", blocking: true, timeout: 0, want: defaultBlockingTimeoutSec * time.Second},
		{name: "background child untimed", blocking: false, timeout: 60},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			childID := newChild(tt.name, tt.blocking, tt.timeout)
			ctx := context.Background()

			newCtx, cancel, aerr := h.mgr.applyChildTimeout(ctx, childID)

			if tt.want == 0 {
				require.ErrorIs(t, aerr, errNoChildTimeout)
				assert.Nil(t, cancel)
				_, ok := newCtx.Deadline()
				assert.False(t, ok, "a background child runs untimed")

				return
			}

			require.NoError(t, aerr)
			require.NotNil(t, cancel)

			defer cancel()

			deadline, ok := newCtx.Deadline()
			require.True(t, ok)
			assert.WithinDuration(t, time.Now().Add(tt.want), deadline, time.Second)
		})
	}
}
