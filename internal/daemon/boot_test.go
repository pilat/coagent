package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/transcript"
)

func TestManager_KillTerminatingOnStartup(t *testing.T) {
	factory := &mockFactory{}
	testHarness := newHarness(t, harnessOptions{configure: withTestModels, clientFor: factory.client})
	mgr := testHarness.mgr
	s := testHarness.store
	ch := mgr.bus.SubscribeAll()

	factory.nextSess = &mockSession{completeAfter: 50 * time.Millisecond}

	ctx := context.Background()
	pid := testProject(t, s, t.TempDir())
	id, err := mgr.Send(ctx, pid, "init", "", nil)
	require.NoError(t, err)

	waitForState(t, ch, id, controllerapi.StateIdle, 3*time.Second)

	// Simulate: Clear set terminating but daemon died before Kill completed
	require.NoError(t, mgr.store.UpdateSessionStatus(
		context.Background(), id, sessionstore.SessionStatusTerminating,
	))

	require.NoError(t, mgr.Start(ctx))

	rec, err := mgr.store.GetSession(context.Background(), id)
	require.NoError(t, err)
	assert.NotNil(t, rec.KilledAt, "terminating session should be killed on startup")
}

func TestSettleUnresolvedCallsDeduplicatesRepeatedCallID(t *testing.T) {
	testFactory := &mockFactory{}
	testHarness := newHarness(t, harnessOptions{configure: withTestModels, clientFor: testFactory.client})
	mgr := testHarness.mgr
	sessions := testHarness.store
	ctx := t.Context()
	projectID := testProject(t, sessions, t.TempDir())
	record, err := sessions.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)
	_, err = sessions.Commit(ctx, sessionstore.Commit{SessionID: record.ID, Messages: []*transcript.Message{
		storedAssistant(`[{"id":"c1","name":"sleep"}]`),
		storedAssistant(`[{"id":"c1","name":"sleep"}]`),
	}})
	require.NoError(t, err)
	require.NoError(t, mgr.settleUnresolvedCalls(ctx))
	pending, err := sessions.ListPending(ctx, record.ID)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, sessionstore.InputSourceCallResult, pending[0].Source)
	assert.Equal(t, "c1", pending[0].Attributes["call_id"])
	assert.Equal(t, tool.IDSleep, pending[0].Attributes["tool_id"])
}

// Only a session that can still ship its transcript needs its calls closed;
// everything else is parked or gone.
func TestOrphanSweepCandidate(t *testing.T) {
	killedAt := time.Now()

	tests := []struct {
		name string
		rec  *sessionstore.SessionRecord
		want bool
	}{
		{name: "active", rec: &sessionstore.SessionRecord{Status: sessionstore.SessionStatusActive}, want: true},
		{name: "suspended", rec: &sessionstore.SessionRecord{Status: sessionstore.SessionStatusSuspended}, want: true},
		{name: "error", rec: &sessionstore.SessionRecord{Status: sessionstore.SessionStatusError}, want: true},
		{name: "completed", rec: &sessionstore.SessionRecord{Status: sessionstore.SessionStatusCompleted}},
		{name: "stopping", rec: &sessionstore.SessionRecord{Status: sessionstore.SessionStatusStopping}},
		{name: "stopped", rec: &sessionstore.SessionRecord{Status: sessionstore.SessionStatusStopped}},
		{name: "terminating", rec: &sessionstore.SessionRecord{Status: sessionstore.SessionStatusTerminating}},
		{
			name: "killed while suspended",
			rec:  &sessionstore.SessionRecord{Status: sessionstore.SessionStatusSuspended, KilledAt: &killedAt},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, orphanSweepCandidate(tt.rec))
		})
	}
}

// The notice tells the model what it can do next, and the terminal prompt is the
// one case where "ask again" is the whole answer.
func TestOrphanedCallNotice(t *testing.T) {
	assert.Contains(t, orphanedCallNotice(tool.IDConfigEdit), "check the current state")
	assert.Contains(t, orphanedCallNotice(tool.IDConfigEdit), "restarted")
}

func TestStartDoesNotLaunchRecoveryAfterShutdown(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	links := &blockingRecoveryLinks{
		Store: h.mgr.links, entered: make(chan struct{}),
		cancelled: make(chan struct{}), allowReturn: make(chan struct{}),
	}
	close(links.allowReturn)
	h.mgr.links = links
	h.mgr.Shutdown(time.Second)

	require.ErrorIs(t, h.mgr.Start(h.ctx), errDaemonShuttingDown)
	assert.Never(t, func() bool {
		select {
		case <-links.entered:
			return true
		default:
			return false
		}
	}, 50*time.Millisecond, 5*time.Millisecond)
}

func TestStartFinishesInterruptedStopBeforeRecoverySweep(t *testing.T) {
	ctx := context.Background()
	testFactory := &mockFactory{}
	testHarness := newHarness(t, harnessOptions{configure: withTestModels, clientFor: testFactory.client})
	mgr := testHarness.mgr
	projects := testHarness.store
	projectID := testProject(t, projects, "/tmp/recover-stop")
	parent, err := mgr.store.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)
	childID, err := mgr.links.Create(ctx, subagent.Create{
		ProjectID: projectID, ParentID: parent.ID, RootID: parent.ID,
		Model: "fake-model", TaskCallID: "background", State: subagent.StateRunning,
	})
	require.NoError(t, err)
	require.NoError(t, mgr.store.UpdateSessionStatus(ctx, parent.ID, sessionstore.SessionStatusStopping))
	require.NoError(t, mgr.store.UpdateSessionStatus(ctx, childID, sessionstore.SessionStatusStopping))

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
		rec, getErr := mgr.store.GetSession(ctx, id)
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
	messages, err := mgr.store.LoadActiveMessages(ctx, parent.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, countToolResultsFor(toDTO(messages), "process_event"))

	mgr.Shutdown(3 * time.Second)
}

// TestSweep_PartialFailureIsNotSuccess: a failed pass used to end in a cheerful
// `sweep_done resumed=0` — crash recovery reporting success it never performed.
func TestSweep_PartialFailureIsNotSuccess(t *testing.T) {
	h := newLedgerHarness(t)
	defer h.shutdown()

	h.flaky.listRunningFail = true

	// Observe at Info: sweep_done is an Info line, so a Warn-level observer would
	// make both assertions vacuously pass.
	core, logs := observer.New(zap.InfoLevel)
	ctx := logger.ToContext(h.ctx, zap.New(core))

	h.startInboxWake()
	h.mgr.resumeAfterRestart(ctx)

	entries := logs.FilterMessage("sweep_incomplete").All()
	require.Len(t, entries, 1)

	fields := entries[0].ContextMap()
	assert.Equal(t, true, fields["running_failed"])
	assert.Equal(t, false, fields["undelivered_failed"], "PASS 2 still ran")

	assert.Empty(t, logs.FilterMessage("sweep_done").All(), "a partial sweep never reports done")
}

// TestSweep_CleanRunReportsDone: the healthy path keeps its existing line.
func TestSweep_CleanRunReportsDone(t *testing.T) {
	h := newLedgerHarness(t)
	defer h.shutdown()

	core, logs := observer.New(zap.InfoLevel)
	ctx := logger.ToContext(h.ctx, zap.New(core))

	h.startInboxWake()
	h.mgr.resumeAfterRestart(ctx)

	assert.Len(t, logs.FilterMessage("sweep_done").All(), 1)
	assert.Empty(t, logs.FilterMessage("sweep_incomplete").All())
}
