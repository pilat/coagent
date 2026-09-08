package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/admission"
	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestScenario_ControlledShutdownRedeliversInterruptedProcess(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "process-restart.db")
	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasToolResultFor(messages, "process_event") {
			return &llmwire.Response{Text: "interruption recovered"}
		}

		return &llmwire.Response{Text: "unexpected activation"}
	}

	h1 := newSubagentHarnessOnDB(t, dbPath, respond, nil)
	service1 := installScenarioProcessService(t, h1)
	root, err := h1.sessStore.CreateSession(h1.ctx, h1.projectID, "fake-model", "", nil)
	require.NoError(t, err)
	require.NoError(t, h1.sessStore.UpdateSessionStatus(h1.ctx, root.ID, sessionstore.SessionStatusCompleted))

	outputPath := filepath.Join(t.TempDir(), "restart.output")
	require.NoError(t, os.WriteFile(outputPath, []byte("partial output\n"), 0o600))
	now := time.Now().UTC()
	require.NoError(t, h1.mgr.processStore.InsertProcess(h1.ctx, backgroundprocess.Process{
		ID: "restart-process", SessionID: root.ID, RootSessionID: root.ID,
		ToolCallID: "restart-call", OutputPath: outputPath, Deadline: now.Add(time.Minute),
		CreatedAt: now, AdvertisedAt: &now, OutputSize: 15, State: backgroundprocess.StateRunning,
	}))
	h1.shutdown()

	record := waitScenarioProcessState(t, h1, "restart-process", backgroundprocess.StateInterrupted)
	assert.Equal(t, "pending", record.DeliveryState)
	_ = service1

	h2 := newSubagentHarnessOnDB(t, dbPath, respond, nil)
	installScenarioProcessService(t, h2)
	collector := collectEvents(h2.mgr.PubSub().SubscribeAll())
	defer func() {
		collector.stop()
		h2.shutdown()
	}()

	require.NoError(t, h2.mgr.Start(h2.ctx))
	waitForVisibleMessage(t, collector, root.ID, "interruption recovered")
	recovered := waitScenarioProcessState(t, h2, "restart-process", backgroundprocess.StateInterrupted)
	assert.Equal(t, "delivered", recovered.DeliveryState)
	assert.Equal(t, 1, processEventCount(t, h2, root.ID))
	assert.Contains(t, lastToolResultContent(h2.parentMessages(root.ID), "process_event"), "partial output")

	require.NoError(t, h2.mgr.recoverProcessInterruptions(h2.ctx))
	assert.Equal(t, 1, processEventCount(t, h2, root.ID))
}

func TestScenario_InterruptedKillSuppressesProcessBeforeRecovery(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "process-kill-restart.db")
	respond := func(string, []llmwire.Message) *llmwire.Response {
		return &llmwire.Response{Text: "must not wake"}
	}

	h1 := newSubagentHarnessOnDB(t, dbPath, respond, nil)
	root, err := h1.sessStore.CreateSession(h1.ctx, h1.projectID, "fake-model", "", nil)
	require.NoError(t, err)
	require.NoError(t, h1.sessStore.UpdateSessionStatus(
		h1.ctx, root.ID, sessionstore.SessionStatusTerminating,
	))

	outputPath := filepath.Join(t.TempDir(), "kill.output")
	require.NoError(t, os.WriteFile(outputPath, []byte("partial output\n"), 0o600))
	now := time.Now().UTC()
	require.NoError(t, h1.mgr.processStore.InsertProcess(h1.ctx, backgroundprocess.Process{
		ID: "killed-process", SessionID: root.ID, RootSessionID: root.ID,
		ToolCallID: "kill-call", OutputPath: outputPath, Deadline: now.Add(time.Minute),
		CreatedAt: now, AdvertisedAt: &now, OutputSize: 15, State: backgroundprocess.StateRunning,
	}))

	h2 := newSubagentHarnessOnDB(t, dbPath, respond, nil)
	installScenarioProcessService(t, h2)
	defer func() {
		h2.shutdown()
		h1.shutdown()
	}()

	require.NoError(t, h2.mgr.Start(h2.ctx))
	record := waitScenarioProcessState(t, h2, "killed-process", backgroundprocess.StateCancelled)
	assert.Equal(t, "suppressed", record.DeliveryState)
	assert.Equal(t, 0, processEventCount(t, h2, root.ID))

	killed, err := h2.sessStore.GetSession(h2.ctx, root.ID)
	require.NoError(t, err)
	assert.NotNil(t, killed.KilledAt)
}

func TestProcessDeliveryFenceSerializesChildTerminalization(t *testing.T) {
	ctx := context.Background()
	mgr, _, projects := newTestManager(t)
	projectID := testProject(t, projects, "/tmp/process-delivery-fence")
	root, err := mgr.sessionStore.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)
	childID := createBackgroundChild(t, mgr, projectID, root.ID)

	entered := make(chan struct{})
	release := make(chan struct{})
	fenced := make(chan error, 1)
	go func() {
		fenced <- mgr.withProcessDeliveryFence(ctx, root.ID, func() error {
			close(entered)
			<-release

			return nil
		})
	}()
	<-entered

	transitioned := make(chan struct{})
	go func() {
		_ = mgr.guardChildTransition(ctx, childID, func(context.Context) error {
			close(transitioned)

			return nil
		})
	}()

	select {
	case <-transitioned:
		t.Fatal("child terminalization crossed the process delivery fence")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	require.NoError(t, <-fenced)
	select {
	case <-transitioned:
	case <-time.After(time.Second):
		t.Fatal("child terminalization did not resume after the delivery fence")
	}
}

func TestProcessDeliveryFenceCarriesClaimedChildIntoNextRunner(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	h := newSubagentHarnessWith(t, func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasToolResultFor(messages, "process_event") {
			close(started)
			<-release

			return &llmwire.Response{Text: "claimed child completion delivered"}
		}

		return &llmwire.Response{Text: "unexpected activation"}
	})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		h.shutdown()
	}()

	root, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)
	childID := createBackgroundChild(t, h.mgr, h.projectID, root.ID)
	path := filepath.Join(t.TempDir(), "claimed.output")
	require.NoError(t, os.WriteFile(path, []byte("done"), 0o600))
	now := time.Now().UTC()
	record := backgroundprocess.Process{
		ID: "claimed-child-process", SessionID: childID, RootSessionID: root.ID,
		ToolCallID: "process-call", OutputPath: path, Deadline: now.Add(time.Minute),
		CreatedAt: now, AdvertisedAt: &now, State: backgroundprocess.StateRunning,
	}
	require.NoError(t, h.mgr.processStore.InsertProcess(h.ctx, record))
	zero := 0
	final, won, err := h.mgr.processStore.Finalize(
		h.ctx, record.ID, backgroundprocess.StateCompleted, &zero, int64(len("done")),
	)
	require.NoError(t, err)
	require.True(t, won)

	require.True(t, h.mgr.admit.TryAdmit(admission.Child, root.ID))
	rs := newRunner(func() {}, t.TempDir(), h.projectID, admission.Child, root.ID, false, nil)
	_, registered := h.mgr.runners.Register(childID, rs)
	require.True(t, registered)

	_, _, err = h.mgr.processCoord.Route(h.ctx, restartCompletion(final))
	require.NoError(t, err)
	require.NoError(t, h.sessStore.UpdateSessionStatus(
		h.ctx, childID, sessionstore.SessionStatusCompleted,
	))
	cancelledCtx, cancel := context.WithCancel(h.ctx)
	cancel()
	errored := false
	h.mgr.finishRunner(cancelledCtx, childID, rs, &errored, false, nil)

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("claimed child completion did not enter the replacement runner")
	}
	link, err := h.mgr.links.GetLink(h.ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.False(t, link.Terminal())
	record = waitScenarioProcessState(t, h, record.ID, backgroundprocess.StateCompleted)
	assert.Equal(t, "delivered", record.DeliveryState)
	assert.Equal(t, 1, processEventCount(t, h, childID))

	close(release)
}

func TestScenario_KillReportsCancelledProcessCount(t *testing.T) {
	h := newSubagentHarnessWith(t, func(string, []llmwire.Message) *llmwire.Response {
		return &llmwire.Response{Text: "ready"}
	})
	service := installScenarioProcessService(t, h)
	defer h.shutdown()

	root, err := h.mgr.Send(h.ctx, h.projectID, "start", "fake-model", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)
	h.waitUntil("root completes initial turn", func() bool {
		record, loadErr := h.sessStore.GetSession(h.ctx, root)

		return loadErr == nil && record.Status == sessionstore.SessionStatusCompleted
	})

	process := startScenarioProcess(t, service, root, root, "sleep 30")
	require.NoError(t, h.mgr.SendToSession(h.ctx, root, "/kill"))
	waitScenarioProcessState(t, h, process.ID, backgroundprocess.StateCancelled)

	var content string
	require.NoError(t, h.db.QueryRowContext(h.ctx, `
		SELECT content FROM session_outbox
		WHERE session_id = ? AND type = 'session_closed'
		ORDER BY id DESC LIMIT 1`, root,
	).Scan(&content))
	assert.Equal(t, "Session killed. Cancelled background processes: 1", content)
}
