package daemon

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/admission"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionlifecycle"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

func TestShieldCommand_IdleToggleStaysOutOfTranscript(t *testing.T) {
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
	bindShieldTestManager(t, mgr)

	require.NoError(t, mgr.SendToSession(ctx, root.ID, "/shieldsup"))
	raised, err := mgr.sessionStore.GetSession(ctx, root.ID)
	require.NoError(t, err)
	assert.True(t, raised.ShieldsUp)
	assert.Equal(t, sessionstore.SessionStatusActive, raised.Status)
	assert.Equal(t, []string{
		sessionstore.ShieldRaiseProgressContent,
		sessionstore.ShieldRaisedContent,
	}, shieldCommandOutputs(t, mgr, root.ID))

	messages, err := mgr.runtimeStore.LoadActiveMessages(ctx, root.ID)
	require.NoError(t, err)
	assert.Empty(t, messages)
	assert.Empty(t, factory.sessions)

	require.NoError(t, mgr.SendToSession(ctx, root.ID, "/shieldsdown"))
	lowered, err := mgr.sessionStore.GetSession(ctx, root.ID)
	require.NoError(t, err)
	assert.False(t, lowered.ShieldsUp)
	assert.Equal(t, []string{sessionstore.ShieldLoweredContent}, shieldCommandOutputs(t, mgr, root.ID))
}

func TestShieldCommand_NextActivationReceivesRaisedPolicy(t *testing.T) {
	mgr, factory, projects := newTestManager(t)
	mgr.sandboxEnabled = true
	ctx := context.Background()
	workDir := t.TempDir()
	projectID := testProject(t, projects, workDir)
	root, _, err := mgr.managerRoots.CreateManagerRoot(ctx, sessionstore.ManagerRootCreate{
		ProjectID: projectID, Model: "model", Attributes: map[string]any{
			controllerapi.SessionAttributeManagerID: "cli",
		}, Name: "project", WorkDir: workDir,
	})
	require.NoError(t, err)

	require.NoError(t, mgr.SendToSession(ctx, root.ID, "/shieldsup"))
	require.NoError(t, mgr.SendToSession(ctx, root.ID, "audit access"))
	t.Cleanup(func() { mgr.Shutdown(3 * time.Second) })
	require.Eventually(t, func() bool {
		factory.mu.Lock()
		defer factory.mu.Unlock()

		return len(factory.options) > 0 && factory.options[len(factory.options)-1].ShieldsUp
	}, 3*time.Second, 10*time.Millisecond)
}

func TestShieldCommand_SpawnedSubagentReceivesRaisedPolicy(t *testing.T) {
	mgr, factory, projects := newTestManager(t)
	mgr.sandboxEnabled = true
	t.Cleanup(func() { mgr.Shutdown(3 * time.Second) })
	ctx := t.Context()
	workDir := t.TempDir()
	projectID := testProject(t, projects, workDir)
	root, _, err := mgr.managerRoots.CreateManagerRoot(ctx, sessionstore.ManagerRootCreate{
		ProjectID: projectID, Model: "model", Attributes: map[string]any{
			controllerapi.SessionAttributeManagerID: "cli",
		}, Name: "project", WorkDir: workDir,
	})
	require.NoError(t, err)
	require.NoError(t, mgr.SendToSession(ctx, root.ID, "/shieldsup"))

	child, err := mgr.Spawn(ctx, spawnRequest{
		ParentID: root.ID, AgentType: "general", Model: "model",
		Prompt: "work", TaskCallID: "task-shield-inheritance",
	})
	require.NoError(t, err)
	childRecord, err := mgr.sessionStore.GetSession(ctx, child.ChildID)
	require.NoError(t, err)
	assert.True(t, childRecord.ShieldsUp)
	require.Eventually(t, func() bool {
		factory.mu.Lock()
		defer factory.mu.Unlock()

		for _, options := range factory.options {
			if options.ID == child.ChildID {
				return options.ShieldsUp
			}
		}

		return false
	}, 3*time.Second, 10*time.Millisecond)
}

func TestShieldCommand_FreshCronActivationKeepsRaisedPolicy(t *testing.T) {
	mgr, factory, projects := newTestManager(t)
	mgr.sandboxEnabled = true
	t.Cleanup(func() { mgr.Shutdown(3 * time.Second) })
	ctx := t.Context()
	workDir := t.TempDir()
	projectID := testProject(t, projects, workDir)
	root, _, err := mgr.managerRoots.CreateManagerRoot(ctx, sessionstore.ManagerRootCreate{
		ProjectID: projectID, Model: "model", Attributes: map[string]any{
			controllerapi.SessionAttributeManagerID: "cli",
		}, Name: "project", WorkDir: workDir,
	})
	require.NoError(t, err)
	require.NoError(t, mgr.SendToSession(ctx, root.ID, "/shieldsup"))

	applied, err := mgr.DeliverFreshSchedule(
		ctx, root.ID, "schedule:cron:9:20260906T1200Z", "run from a blank slate",
	)
	require.NoError(t, err)
	assert.True(t, applied)
	record, err := mgr.sessionStore.GetSession(ctx, root.ID)
	require.NoError(t, err)
	assert.True(t, record.ShieldsUp)

	factory.mu.Lock()
	var optionsID int64
	var optionsShieldsUp bool
	var created *mockSession
	if len(factory.options) > 0 {
		options := factory.options[len(factory.options)-1]
		optionsID = options.ID
		optionsShieldsUp = options.ShieldsUp
	}
	if len(factory.sessions) > 0 {
		created = factory.sessions[len(factory.sessions)-1]
	}
	factory.mu.Unlock()
	require.NotNil(t, created)
	assert.Equal(t, root.ID, optionsID)
	assert.True(t, optionsShieldsUp)
	created.mu.Lock()
	assert.Contains(t, created.inputEvents, "fresh")
	created.mu.Unlock()
}

func TestShieldCommand_RetiresOldSessionPolicyBeforeCompletion(t *testing.T) {
	mgr, factory, projects := newTestManager(t)
	mgr.sandboxEnabled = true
	pool := &recordingPool{}
	mgr.mcpPool = pool
	ctx := context.Background()
	workDir := t.TempDir()
	projectID := testProject(t, projects, workDir)
	root, _, err := mgr.managerRoots.CreateManagerRoot(ctx, sessionstore.ManagerRootCreate{
		ProjectID: projectID, Model: "model", Attributes: map[string]any{
			controllerapi.SessionAttributeManagerID: "cli",
		}, Name: "project", WorkDir: workDir,
	})
	require.NoError(t, err)
	key := fmt.Sprintf("session:%d:false", root.ID)
	factory.processPolicyKey = key
	opened, err := mgr.openSession(ctx, root.ID, workDir, root, false, false)
	require.NoError(t, err)
	opened.Close()
	mgr.recordProcessPolicy(root.ID, "session:settlement:true")

	require.NoError(t, mgr.SendToSession(ctx, root.ID, "/shieldsup"))
	assert.Equal(t, []string{key, "session:settlement:true"}, pool.retired)
	assert.Empty(t, mustInterruptedShieldRaises(t, mgr))
}

func TestShieldCommand_ActiveRaiseParksRunnerWithoutStopOutput(t *testing.T) {
	mgr, _, projects := newTestManager(t)
	mgr.sandboxEnabled = true
	ctx := context.Background()
	projectID := testProject(t, projects, t.TempDir())
	rootID, err := mgr.Send(ctx, projectID, "work", "model", map[string]any{
		controllerapi.SessionAttributeManagerID: "cli",
	})
	require.NoError(t, err)
	bindShieldTestManager(t, mgr)
	require.Eventually(t, func() bool { return mgr.HasActiveLoop(rootID) }, 3*time.Second, 10*time.Millisecond)

	require.NoError(t, mgr.SendToSession(ctx, rootID, "/shieldsup"))
	require.Eventually(t, func() bool { return !mgr.HasActiveLoop(rootID) }, 3*time.Second, 10*time.Millisecond)
	record, err := mgr.sessionStore.GetSession(ctx, rootID)
	require.NoError(t, err)
	assert.True(t, record.ShieldsUp)
	assert.Equal(t, sessionstore.SessionStatusStopped, record.Status)

	allOutputs, outputs := drainShieldCommandOutputs(t, mgr, rootID)
	assert.Equal(t, []string{
		sessionstore.ShieldRaiseProgressContent,
		sessionstore.ShieldRaisedContent,
	}, outputs)
	assert.NotContains(t, allOutputs, sessionstore.StopTerminalContent)

	messages, err := mgr.runtimeStore.LoadActiveMessages(ctx, rootID)
	require.NoError(t, err)
	for _, message := range messages {
		assert.NotEqual(t, "/shieldsup", message.Content)
	}
}

func TestShieldCommand_ActiveRaiseCancelsDescendantWithoutFailure(t *testing.T) {
	mgr, _, projects := newTestManager(t)
	mgr.sandboxEnabled = true
	ctx := context.Background()
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

	registerCancellableShieldRunner(t, mgr, root.ID, workDir, projectID, admission.Parent, 0)
	registerCancellableShieldRunner(t, mgr, childID, workDir, projectID, admission.Child, root.ID)
	require.NoError(t, mgr.SendToSession(ctx, root.ID, "/shieldsup"))

	child, err := mgr.sessionStore.GetSession(ctx, childID)
	require.NoError(t, err)
	assert.True(t, child.ShieldsUp)
	assert.Equal(t, sessionstore.SessionStatusStopped, child.Status)
	link, err := mgr.links.GetLink(ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.Equal(t, subagent.StateStopped, link.State)
	assert.Empty(t, link.Outcome)
}

func TestShieldCommand_ActiveRaisedTreeCannotLowerOrRepark(t *testing.T) {
	for _, command := range []string{"/shieldsdown", "/shieldsup"} {
		t.Run(command, func(t *testing.T) {
			mgr, _, projects := newTestManager(t)
			mgr.sandboxEnabled = true
			ctx := context.Background()
			workDir := t.TempDir()
			projectID := testProject(t, projects, workDir)
			root, _, err := mgr.managerRoots.CreateManagerRoot(ctx, sessionstore.ManagerRootCreate{
				ProjectID: projectID, Model: "model", ShieldsUp: true, Attributes: map[string]any{
					controllerapi.SessionAttributeManagerID: "cli",
				}, Name: "project", WorkDir: workDir,
			})
			require.NoError(t, err)
			bindShieldTestManager(t, mgr)
			registerCancellableShieldRunner(t, mgr, root.ID, workDir, projectID, admission.Parent, 0)

			require.NoError(t, mgr.SendToSession(ctx, root.ID, command))
			assert.True(t, mgr.HasActiveLoop(root.ID))
			record, err := mgr.sessionStore.GetSession(ctx, root.ID)
			require.NoError(t, err)
			assert.True(t, record.ShieldsUp)
			if command == "/shieldsdown" {
				assert.Equal(t, []string{sessionstore.ShieldLowerBusyContent}, shieldCommandOutputs(t, mgr, root.ID))
			} else {
				assert.Equal(t, []string{sessionstore.ShieldRaisedContent}, shieldCommandOutputs(t, mgr, root.ID))
			}

			mgr.Shutdown(3 * time.Second)
		})
	}
}

func registerCancellableShieldRunner(
	t *testing.T,
	mgr *svc,
	sessionID int64,
	workDir string,
	projectID int64,
	kind admission.Kind,
	parentID int64,
) {
	t.Helper()
	runCtx, cancel := context.WithCancel(context.Background())
	runner := sessionlifecycle.NewRunner[queuedSessionInput](
		cancel, workDir, projectID, kind, parentID, false, nil,
	)
	_, registered := mgr.runners.Register(sessionID, runner)
	require.True(t, registered)
	go func() {
		<-runCtx.Done()
		mgr.runners.Delete(sessionID)
		runner.Complete()
	}()
}

func shieldCommandOutputs(t *testing.T, mgr *svc, sessionID int64) []string {
	t.Helper()
	_, outputs := drainShieldCommandOutputs(t, mgr, sessionID)

	return outputs
}

func drainShieldCommandOutputs(t *testing.T, mgr *svc, sessionID int64) ([]string, []string) {
	t.Helper()
	var allOutputs, shieldOutputs []string
	for {
		claim, err := mgr.managerOutputs.ClaimOutputHead(context.Background(), "cli")
		if errors.Is(err, sessionstore.ErrNoOutput) {
			break
		}
		require.NoError(t, err)
		if claim.Output.SessionID == sessionID {
			allOutputs = append(allOutputs, claim.Output.Content)
			if strings.Contains(claim.Output.SourceKey, "shields") {
				shieldOutputs = append(shieldOutputs, claim.Output.Content)
			}
		}
		require.NoError(t, mgr.managerOutputs.AckOutput(
			context.Background(), "cli", claim.Output.ID, claim.Output.AttemptID,
			[]string{strconv.FormatInt(claim.Output.ID, 10)}, nil,
		))
	}

	return allOutputs, shieldOutputs
}

func bindShieldTestManager(t *testing.T, mgr *svc) {
	t.Helper()
	require.NoError(t, mgr.managerOutputs.BindManager(
		context.Background(), "cli", "cli", map[string]any{"local": true},
	))
}
