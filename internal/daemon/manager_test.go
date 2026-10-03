package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

type mockSession struct {
	*scriptedLLM
	mu            sync.Mutex
	ran           bool
	runErr        error
	completeAfter time.Duration
}

type mockFactory struct {
	mu            sync.Mutex
	sessions      []*mockSession
	nextSess      *mockSession
	createErrOnce error
}

func (m *mockSession) Chat(
	ctx context.Context,
	_ string,
	_ []llmwire.Message,
	_ []llmwire.ToolSchema,
	_ ...llmwire.ChatOption,
) (*llmwire.Response, error) {
	m.mu.Lock()
	m.ran = true
	delay, err := m.completeAfter, m.runErr
	m.mu.Unlock()
	if delay <= 0 {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(delay):
	}
	if err != nil {
		return nil, err
	}
	return &llmwire.Response{Text: "done", FinishType: llmwire.FinishStop}, nil
}

func (f *mockFactory) client(*config.Config) (llm.Client, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErrOnce != nil {
		err := f.createErrOnce
		f.createErrOnce = nil
		return nil, err
	}
	client := f.nextSess
	f.nextSess = nil
	if client == nil {
		client = &mockSession{}
	}
	client.scriptedLLM = &scriptedLLM{}
	f.sessions = append(f.sessions, client)
	return client, nil
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

func newTestManager(t *testing.T) (*svc, *mockFactory, *sessionstore.Store) {
	t.Helper()
	mgr, factory, store, _ := newTestManagerWithSchedule(t)
	return mgr, factory, store
}

func newTestManagerWithSchedule(t *testing.T) (*svc, *mockFactory, *sessionstore.Store, schedule.Store) {
	t.Helper()
	restore := coagenthome.Override(t.TempDir())
	t.Cleanup(restore)
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := migrate.OpenDB(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(t.Context(), db, dbPath))
	store := sessionstore.NewStore(db)
	schedules := schedule.NewStore(db, store)
	factory := &mockFactory{}
	cfg := &config.Config{
		Model:   "fake-model",
		WorkDir: t.TempDir(),
		UnifiedConfig: &config.UnifiedConfig{
			Models: []config.ModelEntry{
				{ID: "fake-model"},
				{ID: "test-model"},
				{ID: "my-model"},
				{ID: "old-model"},
				{ID: "new-model"},
			},
		},
	}
	for index := range cfg.UnifiedConfig.Models {
		cfg.UnifiedConfig.Models[index].Reasoning = &config.ReasoningSpec{
			Supported:    true,
			NativeEffort: true,
			Efforts:      []string{"low", "medium", "high"},
		}
		cfg.UnifiedConfig.Models[index].EffortLevels = []string{"low", "medium", "high"}
	}
	in := scriptedBuildInput(t, cfg, store, nil, factory.client)
	mgr, _ := newScenarioDaemon(
		t.Context(),
		in,
		store,
		subagent.NewStore(db, store),
		budget.New(store),
		schedule.NewService(schedules, store),
		func() string { return "fake-model" },
		db,
	)
	t.Cleanup(func() { mgr.Shutdown(3 * time.Second) })
	return mgr, factory, store, schedules
}

func TestManager_Send(t *testing.T) {
	mgr, factory, s := newTestManager(t)
	ch := mgr.bus.SubscribeAll()

	// Use completeAfter so Kill (which no longer cancels context) lets session finish naturally
	factory.nextSess = &mockSession{completeAfter: 200 * time.Millisecond}

	ctx := context.Background()
	pid := testProject(t, s, t.TempDir())
	id, err := mgr.Send(ctx, pid, "hello", "", nil)
	require.NoError(t, err)
	assert.NotEmpty(t, id)

	// Session should be running
	assert.True(t, mgr.HasActiveLoop(id))

	// Kill it
	err = mgr.sendToSession(context.Background(), id, "/kill")
	require.NoError(t, err)

	waitForState(t, ch, id, controllerapi.StateIdle, 3*time.Second)

	assert.False(t, mgr.HasActiveLoop(id))
}

func TestManager_SendToSession_PersistsWhileRunning(t *testing.T) {
	mgr, factory, s := newTestManager(t)

	// Subscribe to all notifications via pubsub
	ch := mgr.bus.SubscribeAll()

	ctx := context.Background()
	pid := testProject(t, s, t.TempDir())
	id, err := mgr.Send(ctx, pid, "init", "", nil)
	require.NoError(t, err)

	// Wait for RunDaemon to start
	waitForLoopStart(t, ch, id, 3*time.Second)
	waitForPendingInput(t, mgr.store.(*sessionstore.Store), id, false, 3*time.Second)

	require.Eventually(
		t,
		func() bool { factory.mu.Lock(); defer factory.mu.Unlock(); return len(factory.sessions) == 1 },
		time.Second,
		10*time.Millisecond,
	)
	factory.mu.Lock()
	require.Len(t, factory.sessions, 1, "factory should have created one session")
	mockSess := factory.sessions[0]
	factory.mu.Unlock()

	mockSess.mu.Lock()
	ran := mockSess.ran
	mockSess.mu.Unlock()
	require.True(t, ran, "RunDaemon should have been called")

	require.NoError(t, mgr.sendToSession(ctx, id, "do something"))
	pending, err := mgr.store.PeekPending(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "do something", pending.RawContent)

	mgr.Shutdown(3 * time.Second)
}

func TestManager_SendDuplicateWorkdir(t *testing.T) {
	mgr, _, s := newTestManager(t)

	ctx := context.Background()
	pid := testProject(t, s, t.TempDir())
	id1, err := mgr.Send(ctx, pid, "init", "", nil)
	require.NoError(t, err)

	// Sending to the same project always creates a new session
	id2, err := mgr.Send(ctx, pid, "another", "", nil)
	require.NoError(t, err)
	assert.NotEqual(t, id1, id2)

	mgr.Shutdown(3 * time.Second)
}

func TestManager_InputReceivedPubSub(t *testing.T) {
	mgr, _, s := newTestManager(t)

	ctx := context.Background()
	pid := testProject(t, s, t.TempDir())
	id, err := mgr.Send(ctx, pid, "init", "", nil)
	require.NoError(t, err)

	sub := mgr.bus.Subscribe(id)
	defer mgr.bus.Unsubscribe(id, sub)

	mgr.bus.Publish(id, sessionevent.Notification{
		Type:    sessionevent.NotifyInputReceived,
		Message: "hello from agent",
		Source:  "agent",
	})

	select {
	case n := <-sub:
		assert.Equal(t, sessionevent.NotifyInputReceived, n.Type)
		assert.Equal(t, "hello from agent", n.Message)
		assert.Equal(t, "agent", n.Source)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for input_received notification")
	}

	mgr.Shutdown(3 * time.Second)
}

// waitForLoopStart blocks until a "running" state_changed notification arrives.
func waitForLoopStart(
	t *testing.T,
	ch <-chan controllerapi.SessionNotification,
	sessionID int64,
	timeout time.Duration,
) {
	t.Helper()
	waitForState(t, ch, sessionID, controllerapi.StateRunning, timeout)
}

func waitForPendingInput(
	t *testing.T,
	store *sessionstore.Store,
	sessionID int64,
	want bool,
	timeout time.Duration,
) {
	t.Helper()
	require.Eventually(t, func() bool {
		_, err := store.PeekPending(context.Background(), sessionID)
		if want {
			return err == nil
		}

		return errors.Is(err, sessionstore.ErrNoPendingInput)
	}, timeout, 10*time.Millisecond)
}

// waitForState blocks until a specific state_changed notification arrives.
func waitForState(
	t *testing.T,
	ch <-chan controllerapi.SessionNotification,
	sessionID int64,
	want controllerapi.State,
	timeout time.Duration,
) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case sn := <-ch:
			if sn.SessionID != sessionID {
				continue
			}
			if sn.Notification.Type == sessionevent.NotifyStateChanged && sn.Notification.Status == want {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for state %q on session %d", want, sessionID)
		}
	}
}

func TestManager_Shutdown(t *testing.T) {
	mgr, _, s := newTestManager(t)

	ctx := context.Background()
	pidA := testProject(t, s, t.TempDir())
	_, err := mgr.Send(ctx, pidA, "init", "", nil)
	require.NoError(t, err)
	pidB := testProject(t, s, t.TempDir())
	_, err = mgr.Send(ctx, pidB, "init", "", nil)
	require.NoError(t, err)

	mgr.Shutdown(5 * time.Second)

	remaining, _ := runnerCounts(mgr.runners)
	assert.Zero(t, remaining, "all loops should be cleaned up after shutdown")
}

// ---------------------------------------------------------------------------
// Goroutine lifecycle tests
// ---------------------------------------------------------------------------

func TestManager_NormalCompletion(t *testing.T) {
	mgr, factory, s := newTestManager(t)
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
	mgr, factory, s := newTestManager(t)
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

func TestManager_GracefulKill(t *testing.T) {
	mgr, factory, s := newTestManager(t)
	ch := mgr.bus.SubscribeAll()

	// Session completes after 200ms — Kill sets killed flag, session finishes naturally
	sess := &mockSession{completeAfter: 200 * time.Millisecond}
	factory.nextSess = sess

	ctx := context.Background()
	pid := testProject(t, s, t.TempDir())
	id, err := mgr.Send(ctx, pid, "init", "", nil)
	require.NoError(t, err)

	waitForLoopStart(t, ch, id, 3*time.Second)

	// Kill sets killed flag — does NOT cancel context
	err = mgr.sendToSession(context.Background(), id, "/kill")
	require.NoError(t, err)

	waitForState(t, ch, id, controllerapi.StateIdle, 3*time.Second)
	assert.False(t, mgr.HasActiveLoop(id))
}

// ---------------------------------------------------------------------------
// Multi-session tests
// ---------------------------------------------------------------------------

func TestManager_SendCreatesNewSessionAfterCompletion(t *testing.T) {
	mgr, factory, s := newTestManager(t)
	ch := mgr.bus.SubscribeAll()

	// First session completes quickly
	factory.nextSess = &mockSession{completeAfter: 50 * time.Millisecond}

	ctx := context.Background()
	pid := testProject(t, s, t.TempDir())
	id1, err := mgr.Send(ctx, pid, "init", "", nil)
	require.NoError(t, err)

	waitForState(t, ch, id1, controllerapi.StateIdle, 3*time.Second)

	// Second Send creates a new session — no resume
	factory.nextSess = &mockSession{completeAfter: 50 * time.Millisecond}
	id2, err := mgr.Send(ctx, pid, "wake up message", "", nil)
	require.NoError(t, err)
	assert.NotEqual(t, id1, id2, "Send should create new session, not resume")

	waitForLoopStart(t, ch, id2, 3*time.Second)

	mgr.Shutdown(3 * time.Second)
}

func TestManager_SendToSession_AlreadyRunningUsesDurableInbox(t *testing.T) {
	mgr, _, s := newTestManager(t)
	ch := mgr.bus.SubscribeAll()

	ctx := context.Background()
	pid := testProject(t, s, t.TempDir())
	id, err := mgr.Send(ctx, pid, "init", "", nil)
	require.NoError(t, err)

	waitForLoopStart(t, ch, id, 3*time.Second)
	waitForPendingInput(t, mgr.store.(*sessionstore.Store), id, false, 3*time.Second)

	// SendToSession on an already-running session — should route to inbox
	err = mgr.sendToSession(ctx, id, "steer message")
	require.NoError(t, err)

	pending, err := mgr.store.PeekPending(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "steer message", pending.RawContent)

	// Only one session should exist
	assert.True(t, mgr.HasActiveLoop(id))

	mgr.Shutdown(3 * time.Second)
}

// ---------------------------------------------------------------------------
// Loop restart on buffered inbox messages
// ---------------------------------------------------------------------------

func TestManager_SecondSendCreatesNewLoop(t *testing.T) {
	mgr, factory, s := newTestManager(t)
	ch := mgr.bus.SubscribeAll()

	factory.nextSess = &mockSession{completeAfter: 50 * time.Millisecond}

	ctx := context.Background()
	pid := testProject(t, s, t.TempDir())
	id1, err := mgr.Send(ctx, pid, "first", "", nil)
	require.NoError(t, err)

	waitForState(t, ch, id1, controllerapi.StateIdle, 3*time.Second)

	// Second Send creates a new session with its own loop
	factory.nextSess = &mockSession{completeAfter: 50 * time.Millisecond}
	id2, err := mgr.Send(ctx, pid, "second", "", nil)
	require.NoError(t, err)
	assert.NotEqual(t, id1, id2, "should create new session")

	waitForLoopStart(t, ch, id2, 3*time.Second)
	require.Eventually(t, func() bool {
		factory.mu.Lock()
		defer factory.mu.Unlock()

		return len(factory.sessions) >= 2
	}, time.Second, 10*time.Millisecond)

	factory.mu.Lock()
	sessCount := len(factory.sessions)
	factory.mu.Unlock()
	assert.GreaterOrEqual(t, sessCount, 2, "factory should have been called at least twice")

	mgr.Shutdown(3 * time.Second)
}

// ---------------------------------------------------------------------------
// Send always creates new session
// ---------------------------------------------------------------------------

func TestManager_SendAlwaysCreatesNew(t *testing.T) {
	mgr, factory, s := newTestManager(t)
	ch := mgr.bus.SubscribeAll()

	// First session completes quickly so Kill can work
	factory.nextSess = &mockSession{completeAfter: 100 * time.Millisecond}

	ctx := context.Background()
	pid := testProject(t, s, t.TempDir())
	id1, err := mgr.Send(ctx, pid, "init", "", nil)
	require.NoError(t, err)

	_ = mgr.sendToSession(context.Background(), id1, "/kill")
	waitForState(t, ch, id1, controllerapi.StateIdle, 3*time.Second)

	// Second Send to same project must create a new session, not resume
	factory.nextSess = &mockSession{completeAfter: 100 * time.Millisecond}
	id2, err := mgr.Send(ctx, pid, "init", "", nil)
	require.NoError(t, err)
	assert.NotEqual(t, id1, id2, "Send should produce a different session ID")

	waitForLoopStart(t, ch, id2, 3*time.Second)

	mgr.Shutdown(3 * time.Second)
}

// ---------------------------------------------------------------------------
// Kill non-running session
// ---------------------------------------------------------------------------

func TestManager_Kill_GracefulRunningSession(t *testing.T) {
	mgr, factory, s := newTestManager(t)
	ch := mgr.bus.SubscribeAll()

	// Session that blocks until context cancelled (Kill calls stop → cancel)
	factory.nextSess = &mockSession{}

	ctx := context.Background()
	pid := testProject(t, s, t.TempDir())
	id, err := mgr.Send(ctx, pid, "init", "", nil)
	require.NoError(t, err)

	waitForLoopStart(t, ch, id, 3*time.Second)

	// Kill is blocking: stop() + mark killed.
	err = mgr.sendToSession(context.Background(), id, "/kill")
	require.NoError(t, err)

	waitForState(t, ch, id, controllerapi.StateIdle, 3*time.Second)

	// Session must be soft-deleted (killed_at set, but still in DB)
	rec, err := mgr.store.GetSession(context.Background(), id)
	require.NoError(t, err)
	assert.NotNil(t, rec.KilledAt, "killed session should have killed_at set")

	// Runner should be cleaned up
	assert.False(t, mgr.HasActiveLoop(id))
}

func TestManager_Kill_NonRunningSession(t *testing.T) {
	mgr, factory, s := newTestManager(t)
	ch := mgr.bus.SubscribeAll()

	factory.nextSess = &mockSession{completeAfter: 50 * time.Millisecond}

	ctx := context.Background()
	pid := testProject(t, s, t.TempDir())
	id, err := mgr.Send(ctx, pid, "init", "", nil)
	require.NoError(t, err)

	waitForState(t, ch, id, controllerapi.StateIdle, 3*time.Second)

	// Session is now idle (not in-memory). Kill should mark it killed.
	err = mgr.sendToSession(context.Background(), id, "/kill")
	require.NoError(t, err)

	rec, err := mgr.store.GetSession(context.Background(), id)
	require.NoError(t, err)
	assert.NotNil(t, rec.KilledAt, "killed non-running session should have killed_at set")
}

// TestManager_Kill_RemovesSchedules: Kill owns schedule teardown — both one-shot
// and cron rows for the killed session are deleted, and other sessions' rows
// survive.
func TestManager_Kill_RemovesSchedules(t *testing.T) {
	mgr, factory, s, schedStore := newTestManagerWithSchedule(t)
	ch := mgr.bus.SubscribeAll()

	factory.nextSess = &mockSession{completeAfter: 50 * time.Millisecond}

	ctx := context.Background()
	pid := testProject(t, s, t.TempDir())
	id, err := mgr.Send(ctx, pid, "init", "", nil)
	require.NoError(t, err)

	waitForState(t, ch, id, controllerapi.StateIdle, 3*time.Second)

	oneShot := time.Now().Add(time.Hour).UTC()
	_, err = schedStore.AddSchedule(ctx, id, "", &oneShot, "one-shot", false)
	require.NoError(t, err)
	_, err = schedStore.AddSchedule(ctx, id, "0 9 * * *", nil, "cron", false)
	require.NoError(t, err)

	factory.nextSess = &mockSession{completeAfter: 50 * time.Millisecond}
	otherID, err := mgr.Send(ctx, pid, "other", "", nil)
	require.NoError(t, err)
	waitForState(t, ch, otherID, controllerapi.StateIdle, 3*time.Second)
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
	mgr, factory, projects, schedStore := newTestManagerWithSchedule(t)
	events := mgr.bus.SubscribeAll()

	factory.nextSess = &mockSession{completeAfter: 50 * time.Millisecond}
	ctx := context.Background()
	projectID := testProject(t, projects, t.TempDir())
	sessionID, err := mgr.Send(ctx, projectID, "init", "", nil)
	require.NoError(t, err)
	waitForState(t, events, sessionID, controllerapi.StateIdle, 3*time.Second)

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
		PendingSleeps(ctx, sessionID)
	require.NoError(t, err)
	assert.Empty(t, pendingSleeps)

	remaining, err := schedStore.ListSchedules(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, remaining, 2)
	assert.Equal(t, "scheduled work", remaining[0].InputMessage())
	assert.Equal(t, "recurring work", remaining[1].InputMessage())
}

// ---------------------------------------------------------------------------
// handleWakeUp schedule storage
// ---------------------------------------------------------------------------

func TestManager_SetModel_IdleSession(t *testing.T) {
	mgr, factory, s := newTestManager(t)
	ch := mgr.bus.SubscribeAll()

	// Session completes quickly → loop exits → session is idle
	factory.nextSess = &mockSession{completeAfter: 50 * time.Millisecond}

	ctx := context.Background()
	pid := testProject(t, s, t.TempDir())
	id, err := mgr.Send(ctx, pid, "init", "old-model", nil)
	require.NoError(t, err)

	waitForState(t, ch, id, controllerapi.StateIdle, 3*time.Second)
	assert.False(t, mgr.HasActiveLoop(id), "session should not be running after idle")

	// SetModel on an idle session must succeed (DB-first pattern)
	err = mgr.SetModel(context.Background(), id, "new-model", "high")
	require.NoError(t, err)

	// Verify the model was persisted in SQLite
	rec, err := mgr.store.GetSession(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, "new-model", rec.Model)
	assert.Equal(t, "high", rec.ReasoningLevel)
}

func TestManager_SendToSession_RejectsKilledSession(t *testing.T) {
	mgr, factory, s := newTestManager(t)
	ch := mgr.bus.SubscribeAll()

	factory.nextSess = &mockSession{completeAfter: 50 * time.Millisecond}

	ctx := context.Background()
	pid := testProject(t, s, t.TempDir())
	id, err := mgr.Send(ctx, pid, "init", "", nil)
	require.NoError(t, err)

	waitForState(t, ch, id, controllerapi.StateIdle, 3*time.Second)

	// Prepare a mock for the kill's resume path
	factory.mu.Lock()
	factory.nextSess = &mockSession{completeAfter: 50 * time.Millisecond}
	factory.mu.Unlock()

	err = mgr.sendToSession(context.Background(), id, "/kill")
	require.NoError(t, err)

	// SendToSession on a killed session must return an error
	err = mgr.sendToSession(ctx, id, "should fail")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "killed")
}

func TestManager_Clear(t *testing.T) {
	mgr, factory, s := newTestManager(t)
	ch := mgr.bus.SubscribeAll()

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

	require.Eventually(t, func() bool {
		return !mgr.HasActiveLoop(id)
	}, 3*time.Second, 10*time.Millisecond)

	// Set reasoning level on the session before clearing
	err = mgr.SetModel(context.Background(), id, "test-model", "high")
	require.NoError(t, err)

	// Clear: creates new session, notifies, then kills old session synchronously
	newID, err := mgr.clear(context.Background(), lifecycleInput(context.Background(), t, mgr, id, "/clear"))
	require.NoError(t, err)
	assert.NotEqual(t, id, newID, "new session should have a different ID")

	// New session should exist with same attributes, model, and reasoning level
	newRec, err := mgr.store.GetSession(context.Background(), newID)
	require.NoError(t, err)
	assert.Nil(t, newRec.KilledAt, "new session should not be killed")
	assert.Equal(t, "test-model", newRec.Model)
	assert.Equal(t, "high", newRec.ReasoningLevel)
	assert.Equal(t, "cli", newRec.Attributes["channel"])
	assert.InDelta(t, float64(42), newRec.Attributes["chat_id"], 0.0)
	assert.Equal(t, "cli", newRec.Attributes[controllerapi.SessionAttributeManagerID])

	// Old session should be killed (Kill ran synchronously inside Clear)
	oldRec, err := mgr.store.GetSession(context.Background(), id)
	require.NoError(t, err)
	assert.NotNil(t, oldRec.KilledAt, "old session should be killed")

	// Verify session.cleared notification was published
	var cleared bool
	deadline := time.After(2 * time.Second)
	for !cleared {
		select {
		case sn := <-ch:
			if sn.Notification.Type == sessionevent.NotifySessionCleared {
				assert.Equal(t, id, sn.Notification.OldSessionID)
				assert.Equal(t, newID, sn.Notification.NewSessionID)
				cleared = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for session.cleared notification")
		}
	}
}

func TestManager_SetAttributesCannotRemoveOrRebindManagerOwner(t *testing.T) {
	mgr, _, store := newTestManager(t)
	ctx := context.Background()
	pid := testProject(t, store, t.TempDir())
	rec, err := mgr.store.CreateSession(ctx, pid, "test-model", "", map[string]any{
		controllerapi.SessionAttributeManagerID: "alpha",
	})
	require.NoError(t, err)

	require.NoError(t, mgr.SetAttributes(ctx, rec.ID, map[string]any{"topic": float64(42)}))
	stored, err := mgr.store.GetSession(ctx, rec.ID)
	require.NoError(t, err)
	assert.Equal(t, "alpha", stored.Attributes[controllerapi.SessionAttributeManagerID])
	assert.InDelta(t, float64(42), stored.Attributes["topic"], 0)

	err = mgr.SetAttributes(ctx, rec.ID, map[string]any{
		controllerapi.SessionAttributeManagerID: "beta",
	})
	require.ErrorContains(t, err, `belongs to manager "alpha"`)
	stored, err = mgr.store.GetSession(ctx, rec.ID)
	require.NoError(t, err)
	assert.Equal(t, "alpha", stored.Attributes[controllerapi.SessionAttributeManagerID])
}

func TestManager_ConcurrentManagerClaimsHaveExactlyOneWinner(t *testing.T) {
	mgr, _, store := newTestManager(t)
	ctx := context.Background()
	pid := testProject(t, store, t.TempDir())
	rec, err := mgr.store.CreateSession(ctx, pid, "test-model", "", nil)
	require.NoError(t, err)

	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, owner := range []string{"alpha", "beta"} {
		go func() {
			<-start
			errs <- mgr.SetAttributes(ctx, rec.ID, map[string]any{
				controllerapi.SessionAttributeManagerID: owner,
			})
		}()
	}
	close(start)

	results := []error{<-errs, <-errs}
	successes := 0
	for _, claimErr := range results {
		if claimErr == nil {
			successes++
		}
	}
	assert.Equal(t, 1, successes)

	stored, err := mgr.store.GetSession(ctx, rec.ID)
	require.NoError(t, err)
	assert.Contains(t, []string{"alpha", "beta"}, stored.Attributes[controllerapi.SessionAttributeManagerID])
}

func TestManager_ClearRejectsAConcurrentLateOwnerClaim(t *testing.T) {
	mgr, _, store := newTestManager(t)
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
		claimResult <- mgr.SetAttributes(ctx, rec.ID, map[string]any{
			controllerapi.SessionAttributeManagerID: "alpha",
		})
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
	mgr, factory, s := newTestManager(t)
	ch := mgr.bus.SubscribeAll()

	// Session that blocks until context cancelled
	factory.nextSess = &mockSession{}

	ctx := context.Background()
	pid := testProject(t, s, t.TempDir())
	id, err := mgr.Send(ctx, pid, "init", "my-model", map[string]any{"lang": "en"})
	require.NoError(t, err)

	waitForLoopStart(t, ch, id, 3*time.Second)

	// Clear: notifies immediately (via pubsub buffer), then kills old session synchronously
	newID, err := mgr.clear(context.Background(), lifecycleInput(context.Background(), t, mgr, id, "/clear"))
	require.NoError(t, err)
	assert.NotEqual(t, id, newID)

	// New session available
	newRec, err := mgr.store.GetSession(context.Background(), newID)
	require.NoError(t, err)
	assert.Nil(t, newRec.KilledAt)
	assert.Equal(t, "my-model", newRec.Model)
	assert.Equal(t, "en", newRec.Attributes["lang"])

	// Runner stopped, old session killed
	assert.False(t, mgr.HasActiveLoop(id))
	oldRec, err := mgr.store.GetSession(context.Background(), id)
	require.NoError(t, err)
	assert.NotNil(t, oldRec.KilledAt)

	// Verify notification was published
	var cleared bool
	deadline := time.After(2 * time.Second)
	for !cleared {
		select {
		case sn := <-ch:
			if sn.Notification.Type == sessionevent.NotifySessionCleared {
				assert.Equal(t, id, sn.Notification.OldSessionID)
				assert.Equal(t, newID, sn.Notification.NewSessionID)
				cleared = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for session.cleared notification")
		}
	}
}

func TestManager_KillTerminatingOnStartup(t *testing.T) {
	mgr, factory, s := newTestManager(t)
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

// ---------------------------------------------------------------------------
// Control-plane tool gating
// ---------------------------------------------------------------------------

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
