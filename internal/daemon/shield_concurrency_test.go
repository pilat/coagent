package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/admission"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

func TestShieldCommand_ActiveRaiseCompletesBeforeQueuedLower(t *testing.T) {
	mgr, _, projects := newTestManager(t)
	mgr.sandboxEnabled = true
	ctx := t.Context()
	workDir := t.TempDir()
	projectID := testProject(t, projects, workDir)
	root, _, err := mgr.managerRoots.CreateManagerRoot(ctx, sessionstore.ManagerRootCreate{
		ProjectID: projectID, Model: "model", Attributes: map[string]any{
			controllerapi.SessionAttributeManagerID: "cli",
		}, Name: "project", WorkDir: workDir,
	})
	require.NoError(t, err)
	bindShieldTestManager(t, mgr)
	registerCancellableShieldRunner(t, mgr, root.ID, workDir, projectID, admission.Parent, 0)

	_, err = mgr.inboxStore.EnqueueInput(ctx, root.ID, sessionstore.InputSourceUser, shieldsUpCommand)
	require.NoError(t, err)
	_, err = mgr.inboxStore.EnqueueInput(ctx, root.ID, sessionstore.InputSourceUser, shieldsDownCommand)
	require.NoError(t, err)

	require.NoError(t, mgr.handlePendingShieldCommands(ctx, root.ID))
	record, err := mgr.sessionStore.GetSession(ctx, root.ID)
	require.NoError(t, err)
	assert.False(t, record.ShieldsUp)
	assert.Equal(t, []string{
		sessionstore.ShieldRaiseProgressContent,
		sessionstore.ShieldRaisedContent,
		sessionstore.ShieldLoweredContent,
	}, shieldCommandOutputs(t, mgr, root.ID))
}

func TestShieldCommand_StopsLiveChildEvenWhenPersistedCompleted(t *testing.T) {
	mgr, _, projects := newTestManager(t)
	mgr.sandboxEnabled = true
	ctx := t.Context()
	workDir := t.TempDir()
	projectID := testProject(t, projects, workDir)
	root, _, err := mgr.managerRoots.CreateManagerRoot(ctx, sessionstore.ManagerRootCreate{
		ProjectID: projectID, Model: "model", Attributes: map[string]any{
			controllerapi.SessionAttributeManagerID: "cli",
		}, Name: "project", WorkDir: workDir,
	})
	require.NoError(t, err)
	childID, err := mgr.subagents.Create(ctx, subagent.Create{
		ProjectID: projectID, ParentID: root.ID, RootID: root.ID, AgentType: "general",
		Model: "model", State: subagent.StateRunning, TaskCallID: "task-1", InitialInput: "work",
	})
	require.NoError(t, err)
	require.NoError(t, mgr.sessionStore.UpdateSessionStatus(ctx, childID, sessionstore.SessionStatusCompleted))
	registerCancellableShieldRunner(t, mgr, root.ID, workDir, projectID, admission.Parent, 0)
	registerCancellableShieldRunner(t, mgr, childID, workDir, projectID, admission.Child, root.ID)

	require.NoError(t, mgr.SendToSession(ctx, root.ID, shieldsUpCommand))
	assert.False(t, mgr.HasActiveLoop(childID))
	child, err := mgr.sessionStore.GetSession(ctx, childID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.SessionStatusStopped, child.Status)
}

func TestSendToChild_WaitingForTreeLockHonorsCancellation(t *testing.T) {
	mgr, _, projects := newTestManager(t)
	ctx := t.Context()
	workDir := t.TempDir()
	projectID := testProject(t, projects, workDir)
	root, _, err := mgr.managerRoots.CreateManagerRoot(ctx, sessionstore.ManagerRootCreate{
		ProjectID: projectID, Model: "model", Attributes: map[string]any{
			controllerapi.SessionAttributeManagerID: "cli",
		}, Name: "project", WorkDir: workDir,
	})
	require.NoError(t, err)
	childID, err := mgr.subagents.Create(ctx, subagent.Create{
		ProjectID: projectID, ParentID: root.ID, RootID: root.ID, AgentType: "general",
		Model: "model", State: subagent.StateRunning, TaskCallID: "task-1", InitialInput: "work",
	})
	require.NoError(t, err)

	unlock, err := mgr.lockSessionTree(ctx, root.ID)
	require.NoError(t, err)
	defer unlock()
	waitCtx, cancel := context.WithCancel(ctx)
	cancel()

	err = mgr.SendToChild(waitCtx, childID, "follow-up")
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	input, err := mgr.inboxStore.PeekPending(ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, input)
	assert.Equal(t, "work", input.RawContent)
}

func TestShieldRecovery_RunningChildStartsOnlyAfterRaisedState(t *testing.T) {
	mgr, factory, projects := newTestManager(t)
	mgr.sandboxEnabled = true
	ctx := t.Context()
	workDir := t.TempDir()
	projectID := testProject(t, projects, workDir)
	root, _, err := mgr.managerRoots.CreateManagerRoot(ctx, sessionstore.ManagerRootCreate{
		ProjectID: projectID, Model: "model", Attributes: map[string]any{
			controllerapi.SessionAttributeManagerID: "cli",
		}, Name: "project", WorkDir: workDir,
	})
	require.NoError(t, err)
	childID, err := mgr.subagents.Create(ctx, subagent.Create{
		ProjectID: projectID, ParentID: root.ID, RootID: root.ID, AgentType: "general",
		Model: "model", State: subagent.StateRunning, TaskCallID: "task-1", InitialInput: "work",
	})
	require.NoError(t, err)
	_, err = mgr.inboxStore.EnqueueInput(ctx, root.ID, sessionstore.InputSourceUser, shieldsUpCommand)
	require.NoError(t, err)

	require.NoError(t, mgr.Start(ctx))
	t.Cleanup(func() { mgr.Shutdown(3 * time.Second) })
	require.Eventually(t, func() bool {
		factory.mu.Lock()
		defer factory.mu.Unlock()
		for _, options := range factory.options {
			if options.ID == childID {
				return options.ShieldsUp
			}
		}
		return false
	}, 3*time.Second, 10*time.Millisecond)
}
