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

// TestDrainPendingRunners_DerivesPromotedRecoveryAfterCapacityWait preserves the
// crash obligation through an admission delay without relying on queue metadata.
func TestDrainPendingRunners_DerivesPromotedRecoveryAfterCapacityWait(t *testing.T) {
	mgr, factory, projects := newTestManager(t)
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
		ctx,
		sessionstore.Commit{
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
	events := mgr.bus.SubscribeAll()

	require.NoError(t, mgr.start(ctx, rec.ID))
	assert.False(t, mgr.HasActiveLoop(rec.ID))
	require.Equal(t, 1, runnerWaitingCount(mgr.runners))

	mgr.runners.release(false, 0)
	reserved--
	mgr.drain(ctx)
	waitForState(t, events, rec.ID, controllerapi.StateIdle, 3*time.Second)

	sess.mu.Lock()
	ran := sess.ran
	sess.mu.Unlock()
	assert.True(t, ran, "first runner must derive and execute the promoted user turn")
}

// A failed classification must preserve the entire waiting FIFO for retry.
func TestDrainQueue_UnknownChildStateDefers(t *testing.T) {
	var flaky *flakyLinkStore

	h := newSubagentHarnessDecorated(t, trivialRespond, func(inner subagent.Store) subagent.Store {
		flaky = newFlakyLinkStore(inner)
		return flaky
	})
	defer h.shutdown()

	parent, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)

	for _, callID := range []string{"bg-1", "bg-2", "bg-3"} {
		childID, cerr := func() (int64, error) {
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
		require.NoError(t, cerr)
		require.NoError(t, seedChildLink(h.ctx, h.sessStore, subagent.Link{
			ParentID: parent.ID, ChildID: childID, TaskCallID: callID,
		}))
		h.mgr.runners.wait(waitingRunner{sessionID: childID, parentID: parent.ID, child: true})
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
	require.Eventually(t, func() bool {
		return h.queueLen() < 3 || runnerLiveCount(h.mgr.runners) > 0
	}, time.Second, 10*time.Millisecond, "the delayed retry must not wait for another slot release")
}

func TestStartSkipsTerminalAndStoppedChildren(t *testing.T) {
	for _, state := range []subagent.State{subagent.StateCompleted, subagent.StateStopped, subagent.StateKilled} {
		t.Run(string(state), func(t *testing.T) {
			mgr, _, projects := newTestManager(t)
			defer mgr.Shutdown(time.Second)

			ctx := context.Background()
			projectID := testProject(t, projects, t.TempDir())
			parent, err := mgr.store.CreateSession(ctx, projectID, "fake-model", "", nil)
			require.NoError(t, err)
			child, err := mgr.store.CreateSession(ctx, projectID, "fake-model", "", nil)
			require.NoError(t, err)
			require.NoError(t, seedChildLink(ctx, projects, subagent.Link{
				ParentID: parent.ID, ChildID: child.ID, TaskCallID: "terminal", State: state,
			}))

			require.NoError(t, mgr.start(ctx, child.ID))
			assert.False(t, mgr.HasActiveLoop(child.ID))
			assert.Zero(t, runnerRunningCount(mgr.runners))
		})
	}
}

func TestStartRejectsKilledSessionWithRunningChildLink(t *testing.T) {
	mgr, _, projects := newTestManager(t)
	defer mgr.Shutdown(time.Second)

	ctx := context.Background()
	projectID := testProject(t, projects, t.TempDir())
	parent, err := mgr.store.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)
	child, err := mgr.store.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)
	require.NoError(t, seedChildLink(ctx, projects, subagent.Link{
		ParentID: parent.ID, ChildID: child.ID, TaskCallID: "running", State: subagent.StateRunning,
	}))
	require.NoError(t, projects.WithTx(ctx, func(tx *sql.Tx) error {
		return sessionstore.MarkSessionKilledTx(ctx, tx, child.ID)
	}))

	err = mgr.start(ctx, child.ID)
	require.ErrorContains(t, err, "killed")
	assert.False(t, mgr.HasActiveLoop(child.ID))
	assert.Zero(t, runnerRunningCount(mgr.runners))
}

// TestScenario_RunnerAddsNoChildLifetimeDeadline uses the raw daemon/session
// seam: the child's own LLM client records the context its Chat calls ran
// under. The runner must hand the child only the explicit cancellation context
// — no wall-clock deadline derived from the link, the agent type, or anything
// else. Explicit stop still reaches that same context.
func TestScenario_RunnerAddsNoChildLifetimeDeadline(t *testing.T) {
	childRelease := make(chan struct{})

	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "CHILD_SEAM") {
			<-childRelease

			return &llmwire.Response{Text: "unreached by the happy path"}
		}

		if hasToolResultFor(msgs, tool.IDTask) {
			return &llmwire.Response{Text: "parent sees the stopped child"}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID:   taskCallID,
			Name: tool.IDTask,
			Arguments: []byte(
				`{"prompt":"CHILD_SEAM","description":"scenario","subagent_type":"general"}`,
			),
		}}}
	}

	h := newSubagentHarnessWith(t, respond)
	defer func() {
		closeOnce(childRelease)
		h.shutdown()
	}()

	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "spawn for seam check", "fake-model", nil)
	require.NoError(t, err)

	link := h.waitForChildLink(parentID)
	childSessionID := fmt.Sprintf("%d:%d", parentID, link.ChildID)

	// Wait until the child's client is actually inside a Chat call, then check
	// the context it was handed: no deadline may ride along.
	h.waitUntil("child Chat is in flight", func() bool {
		client := h.sessionClient(childSessionID)

		return client != nil && client.hasChatContext()
	})
	childClient := h.sessionClient(childSessionID)
	require.NotNil(t, childClient)
	assert.False(t, childClient.chatRanWithDeadline(),
		"the runner must add no child-lifetime deadline")

	// The only interrupt path is explicit stop: it must cancel the very context
	// the child's client holds.
	require.NoError(t, h.mgr.sendToSession(h.ctx, link.ChildID, "/stop"))
	h.waitUntil("stop cancels the child context", childClient.sawCancellation)
	assert.Equal(t, subagent.StateStopped, func() subagent.State {
		l, err := h.links.GetLink(h.ctx, link.ChildID)
		require.NoError(t, err)
		require.NotNil(t, l)

		return l.State
	}(), "an explicit stop parks the child")
}

func TestEnsureRunner_EmptyRootPublishesCreatedAndIdle(t *testing.T) {
	mgr, _, projects := newTestManager(t)
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
	require.Eventually(t, func() bool { return !mgr.HasActiveLoop(rec.ID) }, time.Second, 10*time.Millisecond)
}

func TestStartLockedRejectsShutdown(t *testing.T) {
	h := newSubagentHarness(t)
	h.mgr.life.close()

	err := h.mgr.startLocked(h.ctx, 1)
	require.ErrorIs(t, err, errDaemonShuttingDown)
}

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

// TestDrainQueue_SkipsKilledChild: a queued child cascade-killed before it ran is
// never launched by a subsequent drainQueue.
func TestDrainQueue_SkipsKilledChild(t *testing.T) {
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
		ParentID: parent.ID, ChildID: childID, TaskCallID: "bg",
	}))

	// Park the child, then kill it before any runner picks it up.
	h.mgr.runners.wait(waitingRunner{sessionID: childID, parentID: parent.ID, child: true})
	h.mgr.killSubagent(ctx, childID)

	h.mgr.drain(ctx)

	assert.False(t, h.mgr.HasActiveLoop(childID), "a killed queued child is never launched")
	assert.Equal(t, 0, h.queueLen(), "the killed entry is purged from the queue")
}

// TestAdmissionCaps_ChildrenCappedBelowTotal guards the load-bearing
// deadlock-freedom invariant: children are capped strictly below the total, so at
// least one slot is always reservable by a parent. A completing child can
// therefore always re-admit its suspended (slot-free) parent, killing the
// priority-inversion deadlock. If this relationship ever flips, durable blocking
// fan-in can deadlock — fail loudly at compile/test time instead.
func TestAdmissionCaps_ChildrenCappedBelowTotal(t *testing.T) {
	assert.Less(
		t, maxChildren, maxTotal,
		"maxChildren must stay strictly below maxTotal",
	)
	assert.LessOrEqual(t, maxPerParent, maxChildren, "per-parent cap cannot exceed the child cap")
}
