package daemon

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionevent"
)

func TestManager_NormalCompletion(t *testing.T) {
	factory := &mockFactory{}
	testHarness := newHarness(t, harnessOptions{configure: withTestModels, clientFor: factory.client})
	mgr := testHarness.mgr
	s := testHarness.store
	ch := mgr.bus.SubscribeAll()

	sess := &mockSession{completeAfter: 50 * time.Millisecond}
	factory.nextSess = sess

	ctx := context.Background()
	pid := testProject(t, s, t.TempDir())
	id, err := mgr.Send(ctx, pid, "hi", "", nil)
	require.NoError(t, err)

	waitForState(t, ch, id, controllerapi.StateIdle, 3*time.Second)

	// Verify session removed from in-memory map
	assert.False(t, mgr.HasActiveLoop(id))
}

func TestManager_ErrorPath(t *testing.T) {
	factory := &mockFactory{}
	testHarness := newHarness(t, harnessOptions{configure: withTestModels, clientFor: factory.client})
	mgr := testHarness.mgr
	s := testHarness.store
	ch := mgr.bus.SubscribeAll()

	sess := &mockSession{
		completeAfter: 50 * time.Millisecond,
		runErr:        fmt.Errorf("something went wrong"),
	}
	factory.nextSess = sess

	ctx := context.Background()
	pid := testProject(t, s, t.TempDir())
	id, err := mgr.Send(ctx, pid, "init", "", nil)
	require.NoError(t, err)

	// Wait for the error message and idle state notifications.
	// Errors no longer kill the session — instead the daemon sends an error message
	// and transitions to idle so the session can receive new messages.
	var errMessage string
	var gotIdle bool
	deadline := time.After(3 * time.Second)
	for !gotIdle {
		select {
		case sn := <-ch:
			if sn.SessionID != id {
				continue
			}
			if sn.Notification.Type == sessionevent.NotifyMessage {
				errMessage = sn.Notification.Message
			}
			if sn.Notification.Type == sessionevent.NotifyStateChanged &&
				sn.Notification.Status == controllerapi.StateIdle {
				gotIdle = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for error/idle notifications")
		}
	}

	assert.Contains(t, errMessage, "something went wrong")
}

func TestFinishRunnerCancellationEscapesContendedTreeFence(t *testing.T) {
	ctx := context.Background()
	testFactory := &mockFactory{}
	testHarness := newHarness(t, harnessOptions{configure: withTestModels, clientFor: testFactory.client})
	mgr := testHarness.mgr
	projects := testHarness.store
	projectID := testProject(t, projects, "/tmp/runner-fence-cancel")
	record, err := mgr.store.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)

	unlock, err := mgr.lockSessionTree(ctx, record.ID)
	require.NoError(t, err)

	runnerCtx, cancel := context.WithCancel(ctx)
	rs := newRunner(cancel, t.TempDir(), record, waitingRunner{sessionID: record.ID}, false)
	require.True(t, mgr.runners.tryAdmit(false, 0))
	_, registered := mgr.runners.register(rs)
	require.True(t, registered)

	done := make(chan struct{})
	go func() {
		mgr.finishRunner(runnerCtx, rs, runOutcome{}, nil)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-rs.Done():
	case <-time.After(time.Second):
		unlock()
		t.Fatal("cancelled runner did not complete while the stop fence was held")
	}

	select {
	case <-done:
		unlock()
		t.Fatal("runner finalization crossed the held tree fence")
	default:
	}

	unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runner finalization did not resume after the tree fence")
	}
}

func TestTeardownOnStoreFailureDoesNotPublishIdle(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t, harnessOptions{respond: trivialRespond})
	sessions := h.store
	store := h.store
	projectID := testProject(t, store, "/tmp/teardown-failure")
	record, err := sessions.CreateSession(ctx, projectID, "model", "", map[string]any{
		controllerapi.SessionAttributeManagerID: "manager-teardown",
	})
	require.NoError(t, err)

	failing := &failingGetSessionStore{
		Store: sessions,
		err:   errors.New("disk hiccup"),
	}
	failing.pending.Store(true)
	mgr := h.mgr
	mgr.store = failing
	controllers := newTestController(mgr, &config.Config{}, nil, nil)
	notifications := controllers.ForManager("manager-teardown").Subscribe()

	mgr.publishOwnerlessIdle(ctx, record.ID)
	requireNoManagerNotification(t, notifications)
	mgr.Shutdown(time.Second)
}
