package daemon

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/tool"
)

// Admission delays must preserve the durable recovery obligation without queue metadata.
func TestDrainPendingRunners_DerivesPromotedRecoveryAfterCapacityWait(t *testing.T) {
	factory := &mockFactory{}
	h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: factory.client})
	mgr := h.mgr
	ctx := h.ctx
	reserved := maxTotal
	for range reserved {
		require.True(t, mgr.runners.tryAdmit(false, 0))
	}
	t.Cleanup(func() {
		for range reserved {
			mgr.runners.release(false, 0)
		}
		mgr.Shutdown(t.Context(), 3*time.Second)
	})
	rec := h.createRoot(nil)
	h.acceptUserInput(rec, "promoted before crash", "promoted before crash")
	sess := &mockSession{completeAfter: 10 * time.Millisecond}
	factory.nextSess = sess
	events := collectEvents(t, mgr.bus.SubscribeAll())
	t.Cleanup(events.stop)
	require.NoError(t, mgr.start(ctx, rec))
	assert.False(t, mgr.HasActiveLoop(rec))
	require.Equal(t, 1, runnerWaitingCount(mgr.runners))
	mgr.runners.release(false, 0)
	reserved--
	mgr.drain(ctx)
	events.waitState(rec, controllerapi.StateIdle)
	sess.mu.Lock()
	ran := sess.ran
	sess.mu.Unlock()
	assert.True(t, ran, "first runner must derive and execute the promoted user turn")
}

func TestStartSkipsTerminalAndStoppedChildren(t *testing.T) {
	for _, state := range []subagent.State{subagent.StateCompleted, subagent.StateStopped, subagent.StateKilled} {
		t.Run(string(state), func(t *testing.T) {
			h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: (&mockFactory{}).client})
			mgr := h.mgr
			defer mgr.Shutdown(t.Context(), time.Second)
			ctx := h.ctx
			parent := h.createRoot(nil)
			child := h.createRoot(nil)
			h.attachChildLink(subagent.Link{
				ParentID: parent, ChildID: child, TaskCallID: "terminal", State: state,
			})
			require.NoError(t, mgr.start(ctx, child))
			assert.False(t, mgr.HasActiveLoop(child))
			assert.Zero(t, runnerRunningCount(mgr.runners))
		})
	}
}

func TestStartRejectsKilledSessionWithRunningChildLink(t *testing.T) {
	h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: (&mockFactory{}).client})
	mgr, projects := h.mgr, h.store
	defer mgr.Shutdown(t.Context(), time.Second)
	ctx := h.ctx
	parent := h.createRoot(nil)
	child := h.createRoot(nil)
	h.attachChildLink(subagent.Link{
		ParentID: parent, ChildID: child, TaskCallID: "running", State: subagent.StateRunning,
	})
	require.NoError(t, projects.WithTx(ctx, func(tx *sql.Tx) error {
		return sessionstore.MarkSessionKilledTx(ctx, tx, child)
	}))
	err := mgr.start(ctx, child)
	require.ErrorContains(t, err, "killed")
	assert.False(t, mgr.HasActiveLoop(child))
	assert.Zero(t, runnerRunningCount(mgr.runners))
}

// Child Chat receives cancellation without a wall-clock deadline; explicit stop must cancel that same context.
func TestScenario_RunnerAddsNoChildLifetimeDeadline(t *testing.T) {
	childEntered := make(chan struct{})
	childRelease := make(chan struct{})
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "CHILD_SEAM") {
			closeOnce(childEntered)
			<-childRelease
			return callReply("child-seam-ls", "ls", `{"path":"."}`)
		}
		if hasToolResultFor(msgs, tool.IDTask) {
			return textReply("parent sees the stopped child")
		}
		return callReply(
			taskCallID, tool.IDTask, `{"prompt":"CHILD_SEAM","description":"scenario","subagent_type":"general"}`,
		)
	}
	h := newHarness(t, harnessOptions{respond: respond})
	defer closeOnce(childRelease)
	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "spawn for seam check", "fake-model", nil)
	require.NoError(t, err)
	link := h.waitLink(parentID, taskCallID)
	waitForScenarioSignal(t, childEntered, "child's first model call reached the provider")
	hold := h.holdRunner(link.ChildID)
	close(childRelease)
	waitForScenarioSignal(t, hold.entered, "child Chat is in flight")
	assert.False(t, hold.hadDeadline, "the runner must add no child-lifetime deadline")

	// The only interrupt path is explicit stop: it must cancel the very context the child's client holds.
	closeOnce(hold.release)
	require.NoError(t, h.mgr.sendToSession(h.ctx, link.ChildID, "/stop"))
	waitForScenarioSignal(t, hold.cancelled, "stop cancels the child context")
	assert.Equal(t, subagent.StateStopped, func() subagent.State {
		l := h.link(link.ChildID)
		require.NotNil(t, l)
		return l.State
	}(), "an explicit stop parks the child")
}

func TestEnsureRunner_EmptyRootPublishesCreatedAndIdle(t *testing.T) {
	h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: (&mockFactory{}).client})
	mgr := h.mgr
	ctx := h.ctx
	rec := h.createRoot(nil)
	notifications := mgr.bus.SubscribeAll()
	defer mgr.bus.UnsubscribeAll(notifications)
	require.NoError(t, mgr.start(ctx, rec))
	created := requireNotification(t, notifications)
	assert.Equal(t, sessionevent.NotifySessionCreated, created.Notification.Type)
	idle := requireNotification(t, notifications)
	assert.Equal(t, sessionevent.NotifyStateChanged, idle.Notification.Type)
	assert.Equal(t, controllerapi.StateIdle, idle.Notification.Status)
	h.waitUntil(
		"empty runner idle",
		func() bool { return !mgr.HasActiveLoop(rec) },
	)
}

func TestStartLockedRejectsShutdown(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.mgr.life.close()
	err := h.mgr.startLocked(h.ctx, 1)
	require.ErrorIs(t, err, errDaemonShuttingDown)
}

// Capacity is rechecked against the durable parent after selecting a waiter.
func TestDrainQueue_CapacityReparks(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: trivialRespond})
	parent := h.createRoot(nil)

	// Blocking admission failures expose the re-park branch instead of self-queueing.
	childID := h.createChild(parent, subagent.Link{TaskCallID: "b", Blocking: true})

	// An idle parked parent differs from the link-derived parent, exposing the capacity recheck.
	const idleParentID = int64(9999)
	h.mgr.runners.wait(waitingRunner{sessionID: childID, parentID: idleParentID, child: true})
	for range maxPerParent {
		require.True(t, h.mgr.runners.tryAdmit(true, parent))
	}
	require.True(t, h.mgr.runners.canAdmit(true, idleParentID), "the peek must let this entry through")
	h.mgr.drain(h.ctx)
	assert.Equal(t, 1, h.queueLen(), "a capacity miss parks the child again")
	assert.False(t, h.mgr.HasActiveLoop(childID), "and does not start it")
	for range maxPerParent {
		h.mgr.runners.release(true, parent)
	}
}

// Draining must never launch a queued child killed before admission.
func TestDrainQueue_SkipsKilledChild(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: trivialRespond})
	ctx := h.ctx
	parent := h.createRoot(nil)
	childID := h.createChild(parent, subagent.Link{TaskCallID: "bg"})

	// Park the child, then kill it before any runner picks it up.
	h.mgr.runners.wait(waitingRunner{sessionID: childID, parentID: parent, child: true})
	h.mgr.killSubagent(ctx, childID)
	h.mgr.drain(ctx)
	assert.False(t, h.mgr.HasActiveLoop(childID), "a killed queued child is never launched")
	assert.Equal(t, 0, h.queueLen(), "the killed entry is purged from the queue")
}

// Children must leave a parent slot available so completing a child can re-admit its suspended parent.
func TestAdmissionCaps_ChildrenCappedBelowTotal(t *testing.T) {
	assert.Less(t, maxChildren, maxTotal, "maxChildren must stay strictly below maxTotal")
	assert.LessOrEqual(t, maxPerParent, maxChildren, "per-parent cap cannot exceed the child cap")
}
