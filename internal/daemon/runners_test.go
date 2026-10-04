package daemon

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/tool"
)

func TestRunnerSetRetryBackoff(t *testing.T) {
	t.Parallel()
	set := newRunnerSet()
	for _, want := range []time.Duration{
		100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond,
		1600 * time.Millisecond, 3200 * time.Millisecond, 6400 * time.Millisecond, 12800 * time.Millisecond,
		25600 * time.Millisecond, 30 * time.Second, 30 * time.Second,
	} {
		delay, ok := set.scheduleRetry()
		require.True(t, ok)
		assert.Equal(t, want, delay)
		delay, ok = set.scheduleRetry()
		assert.False(t, ok)
		assert.Zero(t, delay)
		set.retryDone()
	}

	set.resetRetry()
	delay, ok := set.scheduleRetry()
	require.True(t, ok)
	assert.Equal(t, 100*time.Millisecond, delay)
	set.retryDone()
	set.closeAndSnapshot()
	delay, ok = set.scheduleRetry()
	assert.False(t, ok)
	assert.Zero(t, delay)
}

// Admission delays must preserve the durable recovery obligation without queue metadata.
func TestDrainPendingRunners_DerivesPromotedRecoveryAfterCapacityWait(t *testing.T) {
	factory := &mockFactory{}
	h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: factory.client})
	mgr, projects := h.mgr, h.store
	ctx := context.Background()
	reserved := maxTotal
	for range reserved {
		require.True(t, mgr.runners.tryAdmit(false, 0))
	}
	t.Cleanup(func() {
		for range reserved {
			mgr.runners.release(false, 0)
		}
		mgr.Shutdown(3 * time.Second)
	})
	projectID := testProject(t, projects, t.TempDir())
	rec, err := mgr.store.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)
	input, err := mgr.store.Enqueue(
		ctx,
		sessionstore.Input{SessionID: rec.ID, Source: sessionstore.InputSourceUser, Content: "promoted before crash"},
	)
	require.NoError(t, err)
	_, err = mgr.store.Commit(
		ctx, sessionstore.Commit{
			SessionID: input.Input.SessionID,
			Accept: []sessionstore.Accept{
				{
					InputID:    input.Input.ID,
					State:      sessionstore.InputStateAccepted,
					Content:    "promoted before crash",
					LinkRef:    -1,
					ModelBound: true,
				},
			},
		},
	)
	require.NoError(t, err)
	sess := &mockSession{completeAfter: 10 * time.Millisecond}
	factory.nextSess = sess
	events := collectEvents(t, mgr.bus.SubscribeAll())
	t.Cleanup(events.stop)
	require.NoError(t, mgr.start(ctx, rec.ID))
	assert.False(t, mgr.HasActiveLoop(rec.ID))
	require.Equal(t, 1, runnerWaitingCount(mgr.runners))
	mgr.runners.release(false, 0)
	reserved--
	mgr.drain(ctx)
	events.waitFor(t, "session state", func(events []controllerapi.SessionNotification) bool {
		return hasStateEvent(events, rec.ID, controllerapi.StateIdle)
	})
	sess.mu.Lock()
	ran := sess.ran
	sess.mu.Unlock()
	assert.True(t, ran, "first runner must derive and execute the promoted user turn")
}

// A failed classification must preserve the entire waiting FIFO for retry.
func TestDrainQueue_UnknownChildStateDefers(t *testing.T) {
	var flaky *flakyLinkStore
	h := newHarness(t, harnessOptions{respond: trivialRespond, links: func(inner subagent.Store) subagent.Store {
		flaky = newFlakyLinkStore(inner)
		return flaky
	}})
	parent := h.createRoot(nil)
	for _, callID := range []string{"bg-1", "bg-2", "bg-3"} {
		childID := h.createChild(parent, subagent.Link{TaskCallID: callID})
		h.mgr.runners.wait(waitingRunner{sessionID: childID, parentID: parent, child: true})
	}
	require.Equal(t, 3, h.queueLen())
	flaky.failGetLink(1, 0)
	core, logs := observer.New(zap.ErrorLevel)
	ctx := logger.ToContext(h.ctx, zap.New(core))
	h.mgr.drain(ctx)
	assert.Equal(t, 3, h.queueLen(), "nothing is dropped and nothing recursed")
	assert.Zero(t, runnerLiveCount(h.mgr.runners), "no runner was created")
	assert.NotEmpty(t, logs.FilterMessage("waiting_runner_start_failed").All())
	flaky.mu.Lock()
	flaky.getLinkFailFrom = 0
	flaky.mu.Unlock()
	h.waitUntil("delayed admission retry", func() bool {
		return h.queueLen() < 3 || runnerLiveCount(h.mgr.runners) > 0
	})
}

func TestStartSkipsTerminalAndStoppedChildren(t *testing.T) {
	for _, state := range []subagent.State{subagent.StateCompleted, subagent.StateStopped, subagent.StateKilled} {
		t.Run(string(state), func(t *testing.T) {
			h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: (&mockFactory{}).client})
			mgr, projects := h.mgr, h.store
			defer mgr.Shutdown(time.Second)
			ctx := context.Background()
			projectID := testProject(t, projects, t.TempDir())
			parent, err := mgr.store.CreateSession(ctx, projectID, "fake-model", "", nil)
			require.NoError(t, err)
			child, err := mgr.store.CreateSession(ctx, projectID, "fake-model", "", nil)
			require.NoError(t, err)
			h.attachChildLink(subagent.Link{
				ParentID: parent.ID, ChildID: child.ID, TaskCallID: "terminal", State: state,
			})
			require.NoError(t, mgr.start(ctx, child.ID))
			assert.False(t, mgr.HasActiveLoop(child.ID))
			assert.Zero(t, runnerRunningCount(mgr.runners))
		})
	}
}

func TestStartRejectsKilledSessionWithRunningChildLink(t *testing.T) {
	h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: (&mockFactory{}).client})
	mgr, projects := h.mgr, h.store
	defer mgr.Shutdown(time.Second)
	ctx := context.Background()
	projectID := testProject(t, projects, t.TempDir())
	parent, err := mgr.store.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)
	child, err := mgr.store.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)
	h.attachChildLink(subagent.Link{
		ParentID: parent.ID, ChildID: child.ID, TaskCallID: "running", State: subagent.StateRunning,
	})
	require.NoError(t, projects.WithTx(ctx, func(tx *sql.Tx) error {
		return sessionstore.MarkSessionKilledTx(ctx, tx, child.ID)
	}))
	err = mgr.start(ctx, child.ID)
	require.ErrorContains(t, err, "killed")
	assert.False(t, mgr.HasActiveLoop(child.ID))
	assert.Zero(t, runnerRunningCount(mgr.runners))
}

// Child Chat receives cancellation without a wall-clock deadline; explicit stop must cancel that same context.
func TestScenario_RunnerAddsNoChildLifetimeDeadline(t *testing.T) {
	childRelease := make(chan struct{})
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "CHILD_SEAM") {
			<-childRelease
			return textReply("unreached by the happy path")
		}
		if hasToolResultFor(msgs, tool.IDTask) {
			return textReply("parent sees the stopped child")
		}
		return callReply(
			taskCallID, tool.IDTask, `{"prompt":"CHILD_SEAM","description":"scenario","subagent_type":"general"}`,
		)
	}
	h := newHarness(t, harnessOptions{respond: respond})
	defer func() {
		closeOnce(childRelease)
		h.shutdown()
	}()
	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "spawn for seam check", "fake-model", nil)
	require.NoError(t, err)
	h.waitUntil("child link", func() bool { return h.linkByCall(parentID, taskCallID) != nil })
	link := *h.linkByCall(parentID, taskCallID)
	childSessionID := fmt.Sprintf("%d:%d", parentID, link.ChildID)

	// Inspect the context inside Chat; the runner must not add a child lifetime deadline.
	h.waitUntil("child Chat is in flight", func() bool {
		client := h.sessionClient(childSessionID)
		return client != nil && client.hasChatContext()
	})
	childClient := h.sessionClient(childSessionID)
	require.NotNil(t, childClient)
	assert.False(t, childClient.chatRanWithDeadline(), "the runner must add no child-lifetime deadline")

	// The only interrupt path is explicit stop: it must cancel the very context the child's client holds.
	require.NoError(t, h.mgr.sendToSession(h.ctx, link.ChildID, "/stop"))
	h.waitUntil("stop cancels the child context", childClient.sawCancellation)
	assert.Equal(t, subagent.StateStopped, func() subagent.State {
		l := h.link(link.ChildID)
		require.NotNil(t, l)
		return l.State
	}(), "an explicit stop parks the child")
}

func TestEnsureRunner_EmptyRootPublishesCreatedAndIdle(t *testing.T) {
	h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: (&mockFactory{}).client})
	mgr, projects := h.mgr, h.store
	ctx := context.Background()
	workDir := t.TempDir()
	projectID := testProject(t, projects, workDir)
	rec, err := mgr.store.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)
	notifications := mgr.bus.SubscribeAll()
	defer mgr.bus.UnsubscribeAll(notifications)
	require.NoError(t, mgr.start(ctx, rec.ID))
	created := requireNotification(t, notifications)
	assert.Equal(t, sessionevent.NotifySessionCreated, created.Notification.Type)
	idle := requireNotification(t, notifications)
	assert.Equal(t, sessionevent.NotifyStateChanged, idle.Notification.Type)
	assert.Equal(t, controllerapi.StateIdle, idle.Notification.Status)
	h.waitUntil(
		"empty runner idle",
		func() bool { return !mgr.HasActiveLoop(rec.ID) },
	)
}

func TestStartLockedRejectsShutdown(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.mgr.life.close()
	err := h.mgr.startLocked(h.ctx, 1)
	require.ErrorIs(t, err, errDaemonShuttingDown)
}

// An unreadable ledger must not let a child bypass its parent's quota.
func TestEnsureRunner_ClassifyErrorBlocksStart(t *testing.T) {
	var flaky *flakyLinkStore
	h := newHarness(t, harnessOptions{respond: trivialRespond, links: func(inner subagent.Store) subagent.Store {
		flaky = newFlakyLinkStore(inner)
		return flaky
	}})
	var err error
	rec := h.createRoot(nil)
	totalBefore, childrenBefore := runnerRunningCount(h.mgr.runners), runnerChildCount(h.mgr.runners)
	flaky.failGetLink(1, 0)
	err = h.mgr.start(h.ctx, rec)
	require.ErrorIs(t, err, errLinkRead)
	assert.False(t, h.mgr.HasActiveLoop(rec), "no runner for an unclassifiable session")
	assert.Equal(t, totalBefore, runnerRunningCount(h.mgr.runners), "no slot was taken")
	assert.Equal(t, childrenBefore, runnerChildCount(h.mgr.runners))
}

// Failed starts retain their input and wait for the bounded admission retry.
func TestDrainQueue_StartErrorRetainsWaitingInput(t *testing.T) {
	var flaky *flakyLinkStore
	h := newHarness(t, harnessOptions{respond: trivialRespond, links: func(inner subagent.Store) subagent.Store {
		flaky = newFlakyLinkStore(inner)
		return flaky
	}})
	parent := h.createRoot(nil)
	childID := h.createChild(parent, subagent.Link{TaskCallID: "bg"})
	h.mgr.runners.wait(waitingRunner{sessionID: childID, parentID: parent, child: true})
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

// The recorded client exposes the session context actually used by its runner.
func (h *harness) sessionClient(sessionID string) *scriptedLLM {
	h.llmMu.Lock()
	defer h.llmMu.Unlock()
	for _, c := range h.llmRefs {
		c.mu.Lock()
		id := c.sessionID
		c.mu.Unlock()
		if id == sessionID {
			return c
		}
	}
	return nil
}
