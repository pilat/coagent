package daemon

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

func TestManager_GracefulKill(t *testing.T) {
	factory := &mockFactory{}
	h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: factory.client})
	mgr, s := h.mgr, h.store
	ch := collectEvents(t, mgr.bus.SubscribeAll())
	t.Cleanup(ch.stop)

	// Session completes after 200ms — Kill sets killed flag, session finishes naturally
	sess := &mockSession{completeAfter: 200 * time.Millisecond}
	factory.nextSess = sess
	ctx := context.Background()
	pid := testProject(t, s, t.TempDir())
	id, err := mgr.Send(ctx, pid, "init", "", nil)
	require.NoError(t, err)
	ch.waitFor(t, "session state", func(events []controllerapi.SessionNotification) bool {
		return hasStateEvent(events, id, controllerapi.StateRunning)
	})

	// Kill sets killed flag — does NOT cancel context
	err = mgr.sendToSession(context.Background(), id, "/kill")
	require.NoError(t, err)
	ch.waitFor(t, "session state", func(events []controllerapi.SessionNotification) bool {
		return hasStateEvent(events, id, controllerapi.StateIdle)
	})
	assert.False(t, mgr.HasActiveLoop(id))
}

func TestManager_Kill_GracefulRunningSession(t *testing.T) {
	factory := &mockFactory{}
	h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: factory.client})
	mgr, s := h.mgr, h.store
	ch := collectEvents(t, mgr.bus.SubscribeAll())
	t.Cleanup(ch.stop)

	// Session that blocks until context cancelled (Kill calls stop → cancel)
	factory.nextSess = &mockSession{}
	ctx := context.Background()
	pid := testProject(t, s, t.TempDir())
	id, err := mgr.Send(ctx, pid, "init", "", nil)
	require.NoError(t, err)
	ch.waitFor(t, "session state", func(events []controllerapi.SessionNotification) bool {
		return hasStateEvent(events, id, controllerapi.StateRunning)
	})

	// Kill is blocking: stop() + mark killed.
	err = mgr.sendToSession(context.Background(), id, "/kill")
	require.NoError(t, err)
	ch.waitFor(t, "session state", func(events []controllerapi.SessionNotification) bool {
		return hasStateEvent(events, id, controllerapi.StateIdle)
	})

	// Session must be soft-deleted (killed_at set, but still in DB)
	rec := h.session(id)
	assert.NotNil(t, rec.KilledAt, "killed session should have killed_at set")

	// Runner should be cleaned up
	assert.False(t, mgr.HasActiveLoop(id))
}

func TestManager_Kill_NonRunningSession(t *testing.T) {
	factory := &mockFactory{}
	h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: factory.client})
	mgr, s := h.mgr, h.store
	ch := collectEvents(t, mgr.bus.SubscribeAll())
	t.Cleanup(ch.stop)
	factory.nextSess = &mockSession{completeAfter: 50 * time.Millisecond}
	ctx := context.Background()
	pid := testProject(t, s, t.TempDir())
	id, err := mgr.Send(ctx, pid, "init", "", nil)
	require.NoError(t, err)
	ch.waitFor(t, "session state", func(events []controllerapi.SessionNotification) bool {
		return hasStateEvent(events, id, controllerapi.StateIdle)
	})

	// Session is now idle (not in-memory). Kill should mark it killed.
	err = mgr.sendToSession(context.Background(), id, "/kill")
	require.NoError(t, err)
	rec := h.session(id)
	assert.NotNil(t, rec.KilledAt, "killed non-running session should have killed_at set")
}

// Kill removes both schedule kinds for its session while preserving schedules owned by other sessions.
func TestManager_Kill_RemovesSchedules(t *testing.T) {
	factory := &mockFactory{}
	h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: factory.client})
	mgr, s, schedStore := h.mgr, h.store, h.schedules
	ch := collectEvents(t, mgr.bus.SubscribeAll())
	t.Cleanup(ch.stop)
	factory.nextSess = &mockSession{completeAfter: 50 * time.Millisecond}
	ctx := context.Background()
	pid := testProject(t, s, t.TempDir())
	id, err := mgr.Send(ctx, pid, "init", "", nil)
	require.NoError(t, err)
	ch.waitFor(t, "session state", func(events []controllerapi.SessionNotification) bool {
		return hasStateEvent(events, id, controllerapi.StateIdle)
	})
	oneShot := time.Now().Add(time.Hour).UTC()
	_, err = schedStore.AddSchedule(ctx, id, "", &oneShot, "one-shot", false)
	require.NoError(t, err)
	_, err = schedStore.AddSchedule(ctx, id, "0 9 * * *", nil, "cron", false)
	require.NoError(t, err)
	factory.nextSess = &mockSession{completeAfter: 50 * time.Millisecond}
	otherID, err := mgr.Send(ctx, pid, "other", "", nil)
	require.NoError(t, err)
	ch.waitFor(t, "session state", func(events []controllerapi.SessionNotification) bool {
		return hasStateEvent(events, otherID, controllerapi.StateIdle)
	})
	otherOneShot := time.Now().Add(time.Hour).UTC()
	_, err = schedStore.AddSchedule(ctx, otherID, "", &otherOneShot, "untouched", false)
	require.NoError(t, err)
	factory.nextSess = &mockSession{completeAfter: 50 * time.Millisecond}
	require.NoError(t, mgr.sendToSession(context.Background(), id, "/kill"))
	remaining, err := schedStore.ListSchedules(ctx, id)
	require.NoError(t, err)
	assert.Empty(t, remaining, "both one-shot and cron schedules removed on kill")
	untouched, err := schedStore.ListSchedules(ctx, otherID)
	require.NoError(t, err)
	assert.Len(t, untouched, 1, "other session's schedule is untouched")
}

func TestManager_StopCancelsPendingSleepButPreservesScheduledWork(t *testing.T) {
	factory := &mockFactory{}
	h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: factory.client})
	mgr, projects, schedStore := h.mgr, h.store, h.schedules
	events := collectEvents(t, mgr.bus.SubscribeAll())
	t.Cleanup(events.stop)
	factory.nextSess = &mockSession{completeAfter: 50 * time.Millisecond}
	ctx := context.Background()
	projectID := testProject(t, projects, t.TempDir())
	sessionID, err := mgr.Send(ctx, projectID, "init", "", nil)
	require.NoError(t, err)
	events.waitFor(t, "session state", func(events []controllerapi.SessionNotification) bool {
		return hasStateEvent(events, sessionID, controllerapi.StateIdle)
	})
	oneShot := time.Now().Add(time.Hour).UTC()
	_, err = schedStore.AddSchedule(ctx, sessionID, "", &oneShot, "scheduled work", false)
	require.NoError(t, err)
	_, err = schedStore.AddSchedule(ctx, sessionID, "0 9 * * *", nil, "recurring work", false)
	require.NoError(t, err)
	_, err = schedule.NewService(schedStore, mgr.build.Store.(*sessionstore.Store)).AddSleep(
		ctx, sessionID, "sleep-call", time.Now().Add(2*time.Hour).UTC(), "wake",
	)
	require.NoError(t, err)
	require.NoError(t, mgr.sendToSession(ctx, sessionID, "/stop"))
	pendingSleeps, err := schedule.NewService(schedStore, mgr.build.Store.(*sessionstore.Store)).
		PendingSleeps(
			ctx, sessionID,
		)
	require.NoError(t, err)
	assert.Empty(t, pendingSleeps)
	remaining, err := schedStore.ListSchedules(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, remaining, 2)
	assert.Equal(t, "scheduled work", remaining[0].InputMessage())
	assert.Equal(t, "recurring work", remaining[1].InputMessage())
}

func TestManager_Clear(t *testing.T) {
	factory := &mockFactory{}
	h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: factory.client})
	mgr, s := h.mgr, h.store
	ch := collectEvents(t, mgr.bus.SubscribeAll())
	t.Cleanup(ch.stop)

	// Session completes quickly → loop exits → session is idle
	factory.nextSess = &mockSession{completeAfter: 50 * time.Millisecond}
	ctx := context.Background()
	pid := testProject(t, s, t.TempDir())
	id, err := mgr.Send(ctx, pid, "init", "test-model", map[string]any{
		"channel":                               "cli",
		"chat_id":                               float64(42),
		controllerapi.SessionAttributeManagerID: "cli",
	})
	require.NoError(t, err)
	h.waitUntil("TestManager_Clear", func() bool {
		return !mgr.HasActiveLoop(id)
	})

	// Set reasoning level on the session before clearing
	err = mgr.SetModel(context.Background(), id, "test-model", "high")
	require.NoError(t, err)

	// Clear: creates new session, notifies, then kills old session synchronously
	newID, err := mgr.clear(context.Background(), lifecycleInput(context.Background(), t, mgr, id, "/clear"))
	require.NoError(t, err)
	assert.NotEqual(t, id, newID, "new session should have a different ID")

	// New session should exist with same attributes, model, and reasoning level
	newRec := h.session(newID)
	assert.Nil(t, newRec.KilledAt, "new session should not be killed")
	assert.Equal(t, "test-model", newRec.Model)
	assert.Equal(t, "high", newRec.ReasoningLevel)
	assert.Equal(t, "cli", newRec.Attributes["channel"])
	assert.InDelta(t, float64(42), newRec.Attributes["chat_id"], 0.0)
	assert.Equal(t, "cli", newRec.Attributes[controllerapi.SessionAttributeManagerID])

	// Old session should be killed (Kill ran synchronously inside Clear)
	oldRec := h.session(id)
	assert.NotNil(t, oldRec.KilledAt, "old session should be killed")

	// Verify session.cleared notification was published
	ch.waitFor(t, "session cleared", func(events []controllerapi.SessionNotification) bool {
		for _, sn := range events {
			if sn.Notification.Type == sessionevent.NotifySessionCleared {
				assert.Equal(t, id, sn.Notification.OldSessionID)
				assert.Equal(t, newID, sn.Notification.NewSessionID)
				return true
			}
		}
		return false
	})
}

func TestManager_ClearRejectsAConcurrentLateOwnerClaim(t *testing.T) {
	h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: (&mockFactory{}).client})
	mgr, store := h.mgr, h.store
	ctx := context.Background()
	pid := testProject(t, store, t.TempDir())
	rec, err := mgr.store.CreateSession(ctx, pid, "test-model", "", nil)
	require.NoError(t, err)
	blocking := &blockingCreateSessionStore{
		Store:   mgr.store,
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	mgr.store = blocking
	clearResult := make(chan struct {
		id  int64
		err error
	}, 1)
	go func() {
		id, clearErr := mgr.clear(ctx, lifecycleInput(ctx, t, mgr, rec.ID, "/clear"))
		clearResult <- struct {
			id  int64
			err error
		}{id: id, err: clearErr}
	}()
	requireSignal(t, blocking.entered)
	if mgr.routes.claim.TryLock() {
		mgr.routes.claim.Unlock()
		t.Fatal("clear did not hold the manager ownership boundary while creating its replacement")
	}
	claimResult := make(chan error, 1)
	claimStarted := make(chan struct{})
	go func() {
		close(claimStarted)
		claimResult <- mgr.SetAttributes(ctx, rec.ID, managerAttrs("alpha"))
	}()
	requireSignal(t, claimStarted)
	close(blocking.release)
	cleared := <-clearResult
	require.NoError(t, cleared.err)
	require.ErrorContains(t, <-claimResult, "cannot acquire a manager owner")
	replacement, err := mgr.store.GetSession(ctx, cleared.id)
	require.NoError(t, err)
	assert.NotContains(t, replacement.Attributes, controllerapi.SessionAttributeManagerID)
}

func TestManager_ClearWhileRunning(t *testing.T) {
	factory := &mockFactory{}
	h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: factory.client})
	mgr, s := h.mgr, h.store
	ch := collectEvents(t, mgr.bus.SubscribeAll())
	t.Cleanup(ch.stop)

	// Session that blocks until context cancelled
	factory.nextSess = &mockSession{}
	ctx := context.Background()
	pid := testProject(t, s, t.TempDir())
	id, err := mgr.Send(ctx, pid, "init", "my-model", map[string]any{"lang": "en"})
	require.NoError(t, err)
	ch.waitFor(t, "session state", func(events []controllerapi.SessionNotification) bool {
		return hasStateEvent(events, id, controllerapi.StateRunning)
	})

	// Clear: notifies immediately (via pubsub buffer), then kills old session synchronously
	newID, err := mgr.clear(context.Background(), lifecycleInput(context.Background(), t, mgr, id, "/clear"))
	require.NoError(t, err)
	assert.NotEqual(t, id, newID)

	// New session available
	newRec := h.session(newID)
	assert.Nil(t, newRec.KilledAt)
	assert.Equal(t, "my-model", newRec.Model)
	assert.Equal(t, "en", newRec.Attributes["lang"])

	// Runner stopped, old session killed
	assert.False(t, mgr.HasActiveLoop(id))
	oldRec := h.session(id)
	assert.NotNil(t, oldRec.KilledAt)

	// Verify notification was published
	ch.waitFor(t, "session cleared", func(events []controllerapi.SessionNotification) bool {
		for _, sn := range events {
			if sn.Notification.Type == sessionevent.NotifySessionCleared {
				assert.Equal(t, id, sn.Notification.OldSessionID)
				assert.Equal(t, newID, sn.Notification.NewSessionID)
				return true
			}
		}
		return false
	})
}

// A transient store failure while loading the session must not classify an
// owned session as ownerless: the idle publication is skipped, not faked.
func TestStopOnStoreFailureDoesNotPublishIdle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, harnessOptions{respond: trivialRespond})
	sessions, store := h.store, h.store
	projectID := testProject(t, store, "/tmp/stop-failure")
	record, err := sessions.CreateSession(ctx, projectID, "model", "", managerAttrs("manager-stop"))
	require.NoError(t, err)
	failing := &failingGetSessionStore{Store: sessions, err: errors.New("disk hiccup")}
	failing.pending.Store(true)
	// Tree acquisition and the stopped check precede the ownership projection.
	failing.skip = 2
	mgr := h.mgr
	mgr.store = failing
	controllers := newTestController(mgr, &config.Config{}, nil, nil)
	notifications := controllers.ForManager("manager-stop").Subscribe()
	require.NoError(t, mgr.sendToSession(ctx, record.ID, "/stop"),
		"the stop itself must succeed: the cleanup ran on the real store")

	// The unconditional stop announcement is legitimate; the idle publication
	// must be the one thing a failed ownership lookup suppresses.
	stopping := requireManagerNotification(t, notifications)
	require.Equal(t, sessionevent.NotifyMessage, stopping.Notification.Type)
	requireNoManagerNotification(t, notifications)
}

func TestStopParksWholeTreeAndExplicitFollowUpResumesOnlyChild(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: (&mockFactory{}).client})
	mgr, projects := h.mgr, h.store
	projectID := testProject(t, projects, "/tmp/stop-tree")
	parent, err := mgr.store.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)
	childID, err := mgr.links.Create(ctx, subagent.Create{
		ProjectID:  projectID,
		ParentID:   parent.ID,
		RootID:     parent.ID,
		Model:      "fake-model",
		TaskCallID: "blocking-task",
		Blocking:   true,
		State:      subagent.StateRunning,
	})
	require.NoError(t, err)
	_, err = mgr.store.Enqueue(
		ctx, sessionstore.Input{SessionID: childID, Source: sessionstore.InputSourceAgent, Content: "not consumed"},
	)
	require.NoError(t, err)
	require.NoError(t, mgr.sendToSession(ctx, parent.ID, "/stop"))
	for _, id := range []int64{parent.ID, childID} {
		rec := h.session(id)
		assert.Equal(t, sessionstore.SessionStatusStopped, rec.Status)
		_, pendingErr := mgr.store.PeekPending(ctx, id)
		require.ErrorIs(t, pendingErr, sessionstore.ErrNoPendingInput)
	}
	link, err := mgr.links.GetLink(ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.Equal(t, subagent.StateStopped, link.State)
	assert.False(
		t, link.Blocking, "the resolved foreground task becomes an explicitly resumable background continuation",
	)
	require.NoError(t, mgr.SendToChild(ctx, childID, "resume just this child"))
	h.waitUntil("child resumed", func() bool {
		resumed, getErr := mgr.links.GetLink(ctx, childID)
		return getErr == nil && resumed != nil && resumed.State == subagent.StateRunning
	})
	parentRec := h.session(parent.ID)
	assert.Equal(t, sessionstore.SessionStatusStopped, parentRec.Status)
	mgr.Shutdown(3 * time.Second)
}

func TestStopParksActiveDescendantBelowCompletedChild(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: (&mockFactory{}).client})
	mgr, projects := h.mgr, h.store
	projectID := testProject(t, projects, "/tmp/stop-terminal-ancestor")
	root, err := mgr.store.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)
	completedID := createBackgroundChild(t, mgr, projectID, root.ID)
	require.NoError(t, mgr.store.UpdateSessionStatus(ctx, completedID, sessionstore.SessionStatusCompleted))
	activeID := createBackgroundChild(t, mgr, projectID, completedID)
	require.NoError(t, mgr.sendToSession(ctx, root.ID, "/stop"))
	completed := h.session(completedID)
	assert.Equal(t, sessionstore.SessionStatusCompleted, completed.Status)
	active := h.session(activeID)
	assert.Equal(t, sessionstore.SessionStatusStopped, active.Status)
}

func TestStopDirectChildParksItsOwnLinkWithoutStoppingParent(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: (&mockFactory{}).client})
	mgr, projects := h.mgr, h.store
	projectID := testProject(t, projects, "/tmp/stop-direct-child")
	parent, err := mgr.store.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)
	childID, err := mgr.links.Create(ctx, subagent.Create{
		ProjectID: projectID, ParentID: parent.ID, RootID: parent.ID,
		Model: "fake-model", TaskCallID: "background", State: subagent.StateRunning,
	})
	require.NoError(t, err)
	require.NoError(t, mgr.sendToSession(ctx, childID, "/stop"))
	parentRec := h.session(parent.ID)
	assert.Equal(t, sessionstore.SessionStatusActive, parentRec.Status)
	childRec := h.session(childID)
	assert.Equal(t, sessionstore.SessionStatusStopped, childRec.Status)
	link, err := mgr.links.GetLink(ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.Equal(t, subagent.StateStopped, link.State)
}

func TestStopTreeCleanupPreservesBackgroundProcessesForBudgetPark(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: (&mockFactory{}).client})
	mgr, projects := h.mgr, h.store
	projectID := testProject(t, projects, "/tmp/budget-process")
	root, err := mgr.store.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)
	service := backgroundprocess.NewService(mgr.processStore, backgroundprocess.Options{OutputDir: t.TempDir()})
	mgr.processes = service
	process, err := service.Start(ctx, backgroundprocess.Spec{
		ProjectDir: "project-test", SessionID: root.ID, RootSessionID: root.ID, ToolCallID: "budget-process",
		Deadline: time.Minute, Advertise: true,
	}, func(ctx context.Context) (*exec.Cmd, error) {
		return exec.CommandContext(ctx, "sleep", "30"), nil
	})
	require.NoError(t, err)
	require.NoError(t, mgr.stopTreeCleanup(ctx, root.ID, stopTreeOptions{preserveBackgroundProcesses: true}))
	record, err := mgr.processStore.GetProcess(ctx, process.ID)
	require.NoError(t, err)
	assert.Equal(t, backgroundprocess.StateRunning, record.State)
	_, err = service.CancelAll(ctx, backgroundprocess.IntentDaemonShutdown)
	require.NoError(t, err)
}

type blockingCreateSessionStore struct {
	Store
	entered, release chan struct{}
}

func (s *blockingCreateSessionStore) CreateReplacementSession(
	ctx context.Context,
	oldID int64,
) (*sessionstore.SessionRecord, error) {
	close(s.entered)
	<-s.release
	return s.Store.CreateReplacementSession(ctx, oldID)
}
