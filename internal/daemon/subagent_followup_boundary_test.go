package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/admission"
	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

func createBackgroundChild(t *testing.T, mgr *svc, projectID, parentID int64) int64 {
	t.Helper()

	childID, err := mgr.subagents.Create(context.Background(), subagent.Create{
		ProjectID:  projectID,
		ParentID:   parentID,
		RootID:     parentID,
		Model:      "fake-model",
		TaskCallID: "task-follow-up",
		State:      subagent.StateSpawned,
	})
	require.NoError(t, err)

	return childID
}

func TestFollowUpAcceptedBeforeTerminalBoundaryStaysInSameActivation(t *testing.T) {
	ctx := context.Background()
	mgr, _, projects := newTestManager(t)
	projectID := testProject(t, projects, "/tmp/follow-up-boundary")
	parent, err := mgr.sessionStore.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)
	childID := createBackgroundChild(t, mgr, projectID, parent.ID)

	// Keep the accepted child parked so the test can place finalization exactly
	// after the durable enqueue and before any runner promotes the input.
	for i := range admission.MaxChildren {
		require.True(t, mgr.admit.TryAdmit(admission.Child, int64(10_000+i)))
		defer mgr.admit.Release(admission.Child, int64(10_000+i))
	}

	require.NoError(t, mgr.SendToChild(ctx, childID, "one more question"))
	mgr.finalizeChild(ctx, childID)

	link, err := mgr.links.GetLink(ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.False(t, link.Terminal(), "accepted input wins the activation boundary")

	pending, err := mgr.inboxStore.PeekPending(ctx, childID)
	require.NoError(t, err)
	assert.Equal(t, "one more question", pending.RawContent)
}

func TestTerminalChildDeliversPreviousOutcomeBeforeRearm(t *testing.T) {
	ctx := context.Background()
	mgr, _, projects := newTestManager(t)
	projectID := testProject(t, projects, "/tmp/follow-up-rearm")
	parent, err := mgr.sessionStore.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)
	childID := createBackgroundChild(t, mgr, projectID, parent.ID)

	require.NoError(t, mgr.links.MarkLinkTerminal(
		ctx, childID, subagent.StateCompleted, "first outcome", subagent.OutcomeCompleted,
	))
	require.NoError(t, mgr.sessionStore.UpdateSessionStatus(ctx, childID, sessionstore.SessionStatusCompleted))

	require.NoError(t, mgr.SendToChild(ctx, childID, "follow-up after completion"))

	require.Eventually(t, func() bool {
		link, linkErr := mgr.links.GetLink(ctx, childID)
		if linkErr != nil || link == nil || link.State != subagent.StateRunning || link.DeliveredAt != 0 {
			return false
		}

		messages, msgErr := mgr.sessionStore.LoadActiveMessages(ctx, parent.ID)
		if msgErr != nil {
			return false
		}
		for _, message := range messages {
			if message.Role == "user" &&
				containsAll(message.Content, "<subagent_completion>", "first outcome", "outcome: completed") {
				return true
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond)

	mgr.Shutdown(3 * time.Second)
}

func TestProcessInputRearmsCompletedChildAfterPriorOutcomeHandoff(t *testing.T) {
	ctx := context.Background()
	mgr, _, projects := newTestManager(t)
	projectID := testProject(t, projects, "/tmp/process-rearm")
	parent, err := mgr.sessionStore.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)
	childID := createBackgroundChild(t, mgr, projectID, parent.ID)

	for i := range admission.MaxChildren {
		require.True(t, mgr.admit.TryAdmit(admission.Child, int64(20_000+i)))
		defer mgr.admit.Release(admission.Child, int64(20_000+i))
	}

	require.NoError(t, mgr.links.MarkLinkTerminal(
		ctx, childID, subagent.StateCompleted, "first outcome", subagent.OutcomeCompleted,
	))
	require.NoError(t, mgr.sessionStore.UpdateSessionStatus(ctx, childID, sessionstore.SessionStatusCompleted))
	processInput, err := mgr.inboxStore.EnqueueAsyncInput(
		ctx,
		childID,
		sessionstore.InputSourceProcess,
		"<process_completion>late process</process_completion>",
		map[string]any{"process_id": "late-process"},
	)
	require.NoError(t, err)

	require.NoError(t, mgr.inputReady(ctx, childID))

	link, err := mgr.links.GetLink(ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.Equal(t, subagent.StateRunning, link.State)
	assert.Equal(t, int64(2), link.ActivationSeq)
	assert.False(t, link.Blocking)
	assert.Zero(t, link.DeliveredAt)

	pending, err := mgr.inboxStore.PeekPending(ctx, childID)
	require.NoError(t, err)
	assert.Equal(t, processInput.ID, pending.ID)
	assert.Equal(t, sessionstore.InputSourceProcess, pending.Source)

	require.Eventually(t, func() bool {
		messages, msgErr := mgr.sessionStore.LoadActiveMessages(ctx, parent.ID)
		if msgErr != nil {
			return false
		}

		for _, message := range messages {
			if message.Role == "user" &&
				containsAll(message.Content, "<subagent_completion>", "first outcome", "outcome: completed") {
				return true
			}
		}

		return false
	}, 5*time.Second, 10*time.Millisecond)

	mgr.Shutdown(3 * time.Second)
}

func TestProcessInputDoesNotRearmCompletedChildAfterRootStops(t *testing.T) {
	assertProcessInputDoesNotRearmAfterStop(t, false)
}

func TestProcessInputDoesNotRearmCompletedChildAfterDirectStop(t *testing.T) {
	assertProcessInputDoesNotRearmAfterStop(t, true)
}

func assertProcessInputDoesNotRearmAfterStop(t *testing.T, stopChild bool) {
	t.Helper()

	ctx := context.Background()
	mgr, _, projects := newTestManager(t)
	projectID := testProject(t, projects, "/tmp/process-stop-rearm")
	root, err := mgr.sessionStore.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)
	childID := createBackgroundChild(t, mgr, projectID, root.ID)

	require.NoError(t, mgr.links.MarkLinkTerminal(
		ctx, childID, subagent.StateCompleted, "first outcome", subagent.OutcomeCompleted,
	))
	require.NoError(t, mgr.sessionStore.UpdateSessionStatus(ctx, childID, sessionstore.SessionStatusCompleted))
	link, err := mgr.links.GetLink(ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, link)
	won, err := mgr.subagents.DeliverBackgroundCompletion(ctx, *link, 1)
	require.NoError(t, err)
	require.True(t, won)
	_, err = mgr.inboxStore.EnqueueAsyncInput(
		ctx, childID, sessionstore.InputSourceProcess,
		"<process_completion>late process</process_completion>",
		map[string]any{"process_id": "late-process"},
	)
	require.NoError(t, err)

	unlock, err := mgr.lockSessionTree(ctx, root.ID)
	require.NoError(t, err)
	ready := make(chan error, 1)
	go func() { ready <- mgr.inputReady(ctx, childID) }()
	stopID := root.ID
	if stopChild {
		stopID = childID
	}
	require.NoError(t, mgr.sessionStore.UpdateSessionStatus(ctx, stopID, sessionstore.SessionStatusStopped))
	unlock()
	require.NoError(t, <-ready)

	link, err = mgr.links.GetLink(ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.Equal(t, subagent.StateCompleted, link.State)
	assert.Equal(t, int64(1), link.ActivationSeq)
	pending, err := mgr.inboxStore.PeekPending(ctx, childID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.InputSourceProcess, pending.Source)
}

func TestStopParksWholeTreeAndExplicitFollowUpResumesOnlyChild(t *testing.T) {
	ctx := context.Background()
	mgr, _, projects := newTestManager(t)
	projectID := testProject(t, projects, "/tmp/stop-tree")
	parent, err := mgr.sessionStore.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)

	childID, err := mgr.subagents.Create(ctx, subagent.Create{
		ProjectID:  projectID,
		ParentID:   parent.ID,
		RootID:     parent.ID,
		Model:      "fake-model",
		TaskCallID: "blocking-task",
		Blocking:   true,
		State:      subagent.StateRunning,
	})
	require.NoError(t, err)
	_, err = mgr.inboxStore.EnqueueInput(ctx, childID, sessionstore.InputSourceAgent, "not consumed")
	require.NoError(t, err)

	require.NoError(t, mgr.Stop(ctx, parent.ID, 0))

	for _, id := range []int64{parent.ID, childID} {
		rec, getErr := mgr.sessionStore.GetSession(ctx, id)
		require.NoError(t, getErr)
		assert.Equal(t, sessionstore.SessionStatusStopped, rec.Status)
		_, pendingErr := mgr.inboxStore.PeekPending(ctx, id)
		require.ErrorIs(t, pendingErr, sessionstore.ErrNoPendingInput)
	}

	link, err := mgr.links.GetLink(ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.Equal(t, subagent.StateStopped, link.State)
	assert.False(
		t,
		link.Blocking,
		"the resolved foreground task becomes an explicitly resumable background continuation",
	)

	require.NoError(t, mgr.SendToChild(ctx, childID, "resume just this child"))
	require.Eventually(t, func() bool {
		resumed, getErr := mgr.links.GetLink(ctx, childID)
		return getErr == nil && resumed != nil && resumed.State == subagent.StateRunning
	}, 3*time.Second, 10*time.Millisecond)

	parentRec, err := mgr.sessionStore.GetSession(ctx, parent.ID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.SessionStatusStopped, parentRec.Status)

	mgr.Shutdown(3 * time.Second)
}

func TestStopParksActiveDescendantBelowCompletedChild(t *testing.T) {
	ctx := context.Background()
	mgr, _, projects := newTestManager(t)
	projectID := testProject(t, projects, "/tmp/stop-terminal-ancestor")
	root, err := mgr.sessionStore.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)

	completedID := createBackgroundChild(t, mgr, projectID, root.ID)
	require.NoError(t, mgr.sessionStore.UpdateSessionStatus(ctx, completedID, sessionstore.SessionStatusCompleted))
	activeID := createBackgroundChild(t, mgr, projectID, completedID)

	require.NoError(t, mgr.Stop(ctx, root.ID, 0))

	completed, err := mgr.sessionStore.GetSession(ctx, completedID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.SessionStatusCompleted, completed.Status)
	active, err := mgr.sessionStore.GetSession(ctx, activeID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.SessionStatusStopped, active.Status)
}

func TestStopDirectChildParksItsOwnLinkWithoutStoppingParent(t *testing.T) {
	ctx := context.Background()
	mgr, _, projects := newTestManager(t)
	projectID := testProject(t, projects, "/tmp/stop-direct-child")
	parent, err := mgr.sessionStore.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)
	childID, err := mgr.subagents.Create(ctx, subagent.Create{
		ProjectID: projectID, ParentID: parent.ID, RootID: parent.ID,
		Model: "fake-model", TaskCallID: "background", State: subagent.StateRunning,
	})
	require.NoError(t, err)

	require.NoError(t, mgr.Stop(ctx, childID, 0))

	parentRec, err := mgr.sessionStore.GetSession(ctx, parent.ID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.SessionStatusActive, parentRec.Status)
	childRec, err := mgr.sessionStore.GetSession(ctx, childID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.SessionStatusStopped, childRec.Status)
	link, err := mgr.links.GetLink(ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.Equal(t, subagent.StateStopped, link.State)
}

func TestStartFinishesInterruptedStopBeforeRecoverySweep(t *testing.T) {
	ctx := context.Background()
	mgr, _, projects := newTestManager(t)
	projectID := testProject(t, projects, "/tmp/recover-stop")
	parent, err := mgr.sessionStore.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)
	childID, err := mgr.subagents.Create(ctx, subagent.Create{
		ProjectID: projectID, ParentID: parent.ID, RootID: parent.ID,
		Model: "fake-model", TaskCallID: "background", State: subagent.StateRunning,
	})
	require.NoError(t, err)
	require.NoError(t, mgr.sessionStore.UpdateSessionStatus(ctx, parent.ID, sessionstore.SessionStatusStopping))
	require.NoError(t, mgr.sessionStore.UpdateSessionStatus(ctx, childID, sessionstore.SessionStatusStopping))

	outputPath := filepath.Join(t.TempDir(), "stopping.output")
	require.NoError(t, os.WriteFile(outputPath, []byte("partial"), 0o600))
	now := time.Now().UTC()
	require.NoError(t, mgr.processStore.InsertProcess(ctx, backgroundprocess.Process{
		ID: "stopping-process", SessionID: childID, RootSessionID: parent.ID,
		ToolCallID: "stopping-call", OutputPath: outputPath, CreatedAt: now,
		Deadline: now.Add(time.Minute), AdvertisedAt: &now, State: backgroundprocess.StateRunning,
	}))

	require.NoError(t, mgr.Start(ctx))

	for _, id := range []int64{parent.ID, childID} {
		rec, getErr := mgr.sessionStore.GetSession(ctx, id)
		require.NoError(t, getErr)
		assert.Equal(t, sessionstore.SessionStatusStopped, rec.Status)
	}
	link, err := mgr.links.GetLink(ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.Equal(t, subagent.StateStopped, link.State)
	process, err := mgr.processStore.GetProcess(ctx, "stopping-process")
	require.NoError(t, err)
	assert.Equal(t, backgroundprocess.StateCancelled, process.State)
	messages, err := mgr.sessionStore.LoadActiveMessages(ctx, parent.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, countToolResultsFor(toDTO(messages), "process_event"))

	mgr.Shutdown(3 * time.Second)
}

func TestStopTreeCleanupPreservesBackgroundProcessesForBudgetPark(t *testing.T) {
	ctx := context.Background()
	mgr, _, projects := newTestManager(t)
	projectID := testProject(t, projects, "/tmp/budget-process")
	root, err := mgr.sessionStore.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)

	service := backgroundprocess.NewService(mgr.processStore, backgroundprocess.Options{OutputDir: t.TempDir()})
	mgr.processSvc = service
	process, err := service.Start(ctx, backgroundprocess.Spec{
		ProjectDir: "project-test", SessionID: root.ID, RootSessionID: root.ID, ToolCallID: "budget-process",
		Deadline: time.Minute, Advertise: true,
	}, func(ctx context.Context) (*exec.Cmd, error) {
		return exec.CommandContext(ctx, "sleep", "30"), nil
	})
	require.NoError(t, err)

	require.NoError(t, mgr.stopTreeCleanup(ctx, root.ID, stopTreeOptions{
		preserveBackgroundProcesses: true,
	}))
	record, err := mgr.processStore.GetProcess(ctx, process.ID)
	require.NoError(t, err)
	assert.Equal(t, backgroundprocess.StateRunning, record.State)

	_, err = service.CancelAll(ctx, backgroundprocess.IntentDaemonShutdown)
	require.NoError(t, err)
}

func containsAll(value string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(value, needle) {
			return false
		}
	}
	return true
}
