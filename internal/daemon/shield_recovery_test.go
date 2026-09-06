package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestShieldCommand_DisabledSandboxRejectsRaise(t *testing.T) {
	mgr, _, projects := newTestManager(t)
	ctx := context.Background()
	projectID := testProject(t, projects, t.TempDir())
	root, _, err := mgr.managerRoots.CreateManagerRoot(ctx, sessionstore.ManagerRootCreate{
		ProjectID: projectID, Model: "model", Attributes: map[string]any{
			controllerapi.SessionAttributeManagerID: "cli",
		}, Name: "project", WorkDir: t.TempDir(),
	})
	require.NoError(t, err)
	bindShieldTestManager(t, mgr)

	require.NoError(t, mgr.SendToSession(ctx, root.ID, shieldsUpCommand))
	record, err := mgr.sessionStore.GetSession(ctx, root.ID)
	require.NoError(t, err)
	assert.False(t, record.ShieldsUp)
	assert.Equal(t, []string{sessionstore.ShieldSandboxDisabledContent}, shieldCommandOutputs(t, mgr, root.ID))
}

func TestShieldCommand_PendingInputRecoversBeforeRunnerStart(t *testing.T) {
	mgr, factory, projects := newTestManager(t)
	mgr.sandboxEnabled = true
	ctx := context.Background()
	projectID := testProject(t, projects, t.TempDir())
	root, _, err := mgr.managerRoots.CreateManagerRoot(ctx, sessionstore.ManagerRootCreate{
		ProjectID: projectID, Model: "model", Attributes: map[string]any{
			controllerapi.SessionAttributeManagerID: "cli",
		}, Name: "project", WorkDir: t.TempDir(),
	})
	require.NoError(t, err)
	require.NoError(t, mgr.sessionStore.UpdateSessionStatus(ctx, root.ID, sessionstore.SessionStatusStopped))
	_, err = mgr.inboxStore.EnqueueInput(ctx, root.ID, sessionstore.InputSourceUser, shieldsUpCommand)
	require.NoError(t, err)

	require.NoError(t, mgr.Start(ctx))
	t.Cleanup(func() { mgr.Shutdown(3 * time.Second) })
	require.Eventually(t, func() bool {
		record, err := mgr.sessionStore.GetSession(ctx, root.ID)
		return err == nil && record.ShieldsUp
	}, 3*time.Second, 10*time.Millisecond)
	assert.Empty(t, factory.sessions)
}

func TestShieldCommand_InterruptedActiveRaiseCompletesOnStart(t *testing.T) {
	mgr, _, projects := newTestManager(t)
	mgr.sandboxEnabled = true
	ctx := context.Background()
	projectID := testProject(t, projects, t.TempDir())
	root, _, err := mgr.managerRoots.CreateManagerRoot(ctx, sessionstore.ManagerRootCreate{
		ProjectID: projectID, Model: "model", Attributes: map[string]any{
			controllerapi.SessionAttributeManagerID: "cli",
		}, Name: "project", WorkDir: t.TempDir(),
	})
	require.NoError(t, err)
	input, err := mgr.inboxStore.EnqueueInput(ctx, root.ID, sessionstore.InputSourceUser, shieldsUpCommand)
	require.NoError(t, err)
	_, _, err = mgr.lifecycleStore.BeginShieldRaise(ctx, input.ID, true, true)
	require.NoError(t, err)

	require.NoError(t, mgr.Start(ctx))
	t.Cleanup(func() { mgr.Shutdown(3 * time.Second) })
	record, err := mgr.sessionStore.GetSession(ctx, root.ID)
	require.NoError(t, err)
	assert.True(t, record.ShieldsUp)
	assert.Equal(t, sessionstore.SessionStatusStopped, record.Status)
	assert.Empty(t, mustInterruptedShieldRaises(t, mgr))
}

func TestShieldCommand_InterruptedIdleRaiseCompletesBeforePendingDown(t *testing.T) {
	mgr, _, projects := newTestManager(t)
	mgr.sandboxEnabled = true
	ctx := context.Background()
	projectID := testProject(t, projects, t.TempDir())
	root, _, err := mgr.managerRoots.CreateManagerRoot(ctx, sessionstore.ManagerRootCreate{
		ProjectID: projectID, Model: "model", Attributes: map[string]any{
			controllerapi.SessionAttributeManagerID: "cli",
		}, Name: "project", WorkDir: t.TempDir(),
	})
	require.NoError(t, err)
	bindShieldTestManager(t, mgr)
	raiseInput, err := mgr.inboxStore.EnqueueInput(ctx, root.ID, sessionstore.InputSourceUser, shieldsUpCommand)
	require.NoError(t, err)
	_, _, err = mgr.lifecycleStore.BeginShieldRaise(ctx, raiseInput.ID, false, true)
	require.NoError(t, err)
	_, err = mgr.inboxStore.EnqueueInput(ctx, root.ID, sessionstore.InputSourceUser, shieldsDownCommand)
	require.NoError(t, err)

	require.NoError(t, mgr.Start(ctx))
	t.Cleanup(func() { mgr.Shutdown(3 * time.Second) })
	record, err := mgr.sessionStore.GetSession(ctx, root.ID)
	require.NoError(t, err)
	assert.False(t, record.ShieldsUp)
	assert.Empty(t, mustInterruptedShieldRaises(t, mgr))
	assert.Equal(t, []string{
		sessionstore.ShieldRaiseProgressContent,
		sessionstore.ShieldRaisedContent,
		sessionstore.ShieldLoweredContent,
	}, shieldCommandOutputs(t, mgr, root.ID))
}

func TestShieldCommand_DurableOrderControlsFinalState(t *testing.T) {
	mgr, _, projects := newTestManager(t)
	mgr.sandboxEnabled = true
	ctx := context.Background()
	projectID := testProject(t, projects, t.TempDir())
	root, _, err := mgr.managerRoots.CreateManagerRoot(ctx, sessionstore.ManagerRootCreate{
		ProjectID: projectID, Model: "model", Attributes: map[string]any{
			controllerapi.SessionAttributeManagerID: "cli",
		}, Name: "project", WorkDir: t.TempDir(),
	})
	require.NoError(t, err)
	_, err = mgr.inboxStore.EnqueueInput(ctx, root.ID, sessionstore.InputSourceUser, shieldsUpCommand)
	require.NoError(t, err)
	_, err = mgr.inboxStore.EnqueueInput(ctx, root.ID, sessionstore.InputSourceUser, shieldsDownCommand)
	require.NoError(t, err)

	require.NoError(t, mgr.handlePendingShieldCommands(ctx, root.ID))
	record, err := mgr.sessionStore.GetSession(ctx, root.ID)
	require.NoError(t, err)
	assert.False(t, record.ShieldsUp)
}

func mustInterruptedShieldRaises(t *testing.T, mgr *svc) []sessionstore.InterruptedShieldRaise {
	t.Helper()
	raises, err := mgr.lifecycleStore.SelectInterruptedShieldRaises(context.Background())
	require.NoError(t, err)

	return raises
}
