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

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/tool"
)

// mockSession implements session.Service for testing.
type mockSession struct {
	runErr        error
	injectErr     error // returned by every Inject*/Reset* call
	result        session.RunResult
	mu            sync.Mutex
	ran           bool
	registry      tool.Registry
	completeAfter time.Duration // if > 0, RunDaemon returns after this delay instead of blocking
	closeCalled   bool
	agentType     registry.AgentType // defaults to registry.AgentTypeBuild when unset
	prepare       func(string) (string, error)

	pendingCalls []session.PendingToolCall
	inputEvents  []string
	pendingWork  bool
	boundary     session.InputBoundary
}

func (m *mockSession) SettleStoppedCalls(context.Context, string) error { return nil }

type blockingCreateSessionStore struct {
	sessionstore.OrchestrationStore
	entered chan struct{}
	release chan struct{}
}

func (s *blockingCreateSessionStore) CreateSession(
	ctx context.Context,
	projectID int64,
	model, reasoningLevel string,
	attrs map[string]any,
) (*sessionstore.SessionRecord, error) {
	close(s.entered)
	<-s.release

	return s.OrchestrationStore.CreateSession(ctx, projectID, model, reasoningLevel, attrs)
}

// mockFactory implements session.Factory for testing.
type mockFactory struct {
	mu       sync.Mutex
	sessions []*mockSession
	nextSess session.Service // allows injecting any session.Service implementation
}

func (m *mockSession) RunDaemon(
	ctx context.Context,
	notify func(sessionevent.Notification),
) (session.RunResult, error) {
	m.mu.Lock()
	m.ran = true
	m.pendingWork = false
	completeAfter := m.completeAfter
	boundary := m.boundary
	pendingCalls := append([]session.PendingToolCall(nil), m.pendingCalls...)
	m.mu.Unlock()

	if err := m.acceptBoundaryInput(ctx, boundary, pendingCalls); err != nil {
		return session.RunResult{}, err
	}

	// If completeAfter is set, return naturally after the delay.
	if completeAfter > 0 {
		select {
		case <-time.After(completeAfter):
			return m.result, m.runErr
		case <-ctx.Done():
			return m.result, ctx.Err()
		}
	}

	<-ctx.Done()

	return m.result, ctx.Err()
}

func (m *mockSession) acceptBoundaryInput(
	ctx context.Context,
	boundary session.InputBoundary,
	pendingCalls []session.PendingToolCall,
) error {
	if boundary == nil {
		return nil
	}

	input, err := boundary.Peek(ctx)
	if err != nil || input == nil {
		return err
	}

	prepared, err := m.PrepareUserMessage(input.Content)
	if err != nil {
		return err
	}

	_, _, err = boundary.Accept(ctx, *input, prepared, pendingCalls)

	return err
}

func (m *mockSession) PrepareUserMessage(message string) (string, error) {
	if m.prepare != nil {
		return m.prepare(message)
	}

	return message, nil
}

func (m *mockSession) SetModel(_, _ string) error { return nil }

func (m *mockSession) AgentTypes() *registry.Set { return registry.NewSet(nil) }

// RegisterGatedTool mirrors *svc's gating: register onto the mock's internal
// registry only if the mock's agent type allows it.
func (m *mockSession) RegisterGatedTool(t tool.Tool) bool {
	agentType := m.agentType
	if agentType == "" {
		agentType = registry.AgentTypeBuild
	}

	if len(m.AgentTypes().FilterTools([]string{t.ID()}, agentType)) == 0 {
		return false
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.registry == nil {
		m.registry = tool.NewRegistry()
	}
	m.registry.Register(t)

	return true
}

// hasTool reports whether id was registered via RegisterGatedTool.
func (m *mockSession) hasTool(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.registry != nil && m.registry.Get(id) != nil
}

func (m *mockSession) PendingExternalCalls() []session.PendingToolCall {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]session.PendingToolCall(nil), m.pendingCalls...)
}

func (m *mockSession) ResolvePendingCall(
	_ context.Context,
	call session.PendingToolCall,
	_ string,
) (session.CallResolution, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.injectErr != nil {
		return 0, m.injectErr
	}

	for i, pending := range m.pendingCalls {
		if pending == call {
			m.pendingCalls = append(m.pendingCalls[:i], m.pendingCalls[i+1:]...)
			m.inputEvents = append(m.inputEvents, "resolve:"+call.ID)
			return session.CallResolutionInserted, nil
		}
	}

	return session.CallResolutionAlreadyPresent, nil
}

func (m *mockSession) InjectToolNotification(_ context.Context, toolName, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.injectErr == nil {
		m.inputEvents = append(m.inputEvents, "event:"+toolName)
	}

	return m.injectErr
}

func (m *mockSession) InjectToolNotificationOnce(
	ctx context.Context,
	_, toolName, content string,
) (bool, error) {
	err := m.InjectToolNotification(ctx, toolName, content)
	return err == nil, err
}

func (m *mockSession) ResetContextAndInject(_ context.Context, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.injectErr == nil {
		m.inputEvents = append(m.inputEvents, "fresh")
	}

	return m.injectErr
}

func (m *mockSession) ResetContextAndInjectOnce(ctx context.Context, _, prompt string) (bool, error) {
	err := m.ResetContextAndInject(ctx, prompt)
	return err == nil, err
}
func (m *mockSession) ReloadDeliveredCompletion(context.Context) error { return nil }
func (m *mockSession) HasPendingWork() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pendingWork
}

func (m *mockSession) Close() {
	m.mu.Lock()
	m.closeCalled = true
	m.mu.Unlock()
}

func (f *mockFactory) Create(ctx context.Context, opts session.CreateOptions) (session.Service, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	// The production session consumes durable input at its loop boundary. These
	// daemon tests use a minimal mock service, so model that boundary here to keep
	// the persistence/runner lifecycle realistic without running an LLM loop.
	pending := false
	if opts.InputBoundary != nil {
		if input, err := opts.InputBoundary.Peek(ctx); err == nil && input != nil {
			pending = true
		}
	}

	if f.nextSess != nil {
		s := f.nextSess
		f.nextSess = nil
		if ms, ok := s.(*mockSession); ok {
			ms.pendingWork = pending
			ms.boundary = opts.InputBoundary
			f.sessions = append(f.sessions, ms)
		}
		return s, nil
	}

	s := &mockSession{pendingWork: pending, boundary: opts.InputBoundary}
	f.sessions = append(f.sessions, s)
	return s, nil
}

func newTestManager(t *testing.T) (*svc, *mockFactory, Store) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := migrate.OpenDB(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	require.NoError(t, migrate.Run(context.Background(), db, dbPath))
	store := NewStore(db)
	sessStore := sessionstore.NewStore(db)

	factory := &mockFactory{}
	mgr := newSvc(
		factory,
		store,
		sessStore,
		sessStore,
		sessStore,
		sessStore,
		sessStore,
		sessStore,
		sessStore,
		subagent.NewStore(db),
		subagent.NewTransactions(db),
		nil,
		sessStore,
		nil,
		nil,
	)
	return mgr, factory, store
}

// newTestManagerWithSchedule wires a real schedule.Service (sharing the manager's
// DB) instead of the nil used by newTestManager, so kill-path schedule cleanup can
// be exercised and asserted against directly-inserted schedule rows.
func newTestManagerWithSchedule(t *testing.T) (*svc, *mockFactory, Store, schedule.Store) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := migrate.OpenDB(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	require.NoError(t, migrate.Run(context.Background(), db, dbPath))
	store := NewStore(db)
	sessStore := sessionstore.NewStore(db)
	schedStore := schedule.NewStore(db)

	factory := &mockFactory{}
	mgr := newSvc(
		factory, store, sessStore, sessStore, sessStore,
		sessStore, sessStore, sessStore, sessStore,
		subagent.NewStore(db), subagent.NewTransactions(db),
		nil, sessStore, schedule.NewService(schedStore), nil,
	)
	return mgr, factory, store, schedStore
}

func TestManager_Send(t *testing.T) {
	mgr, factory, s := newTestManager(t)
	ch := mgr.PubSub().SubscribeAll()

	// Use completeAfter so Kill (which no longer cancels context) lets session finish naturally
	factory.nextSess = &mockSession{completeAfter: 200 * time.Millisecond}

	ctx := context.Background()
	pid := testProject(t, s, "/tmp/test")
	id, err := mgr.Send(ctx, pid, "hello", "", nil)
	require.NoError(t, err)
	assert.NotEmpty(t, id)

	// Session should be running
	assert.True(t, mgr.HasActiveLoop(id))

	// Kill it
	err = mgr.Kill(context.Background(), id)
	require.NoError(t, err)

	waitForState(t, ch, id, controllerapi.StateIdle, 3*time.Second)

	assert.False(t, mgr.HasActiveLoop(id))
}

func TestManager_SendToSession_PersistsWhileRunning(t *testing.T) {
	mgr, factory, s := newTestManager(t)

	// Subscribe to all notifications via pubsub
	ch := mgr.PubSub().SubscribeAll()

	ctx := context.Background()
	pid := testProject(t, s, "/tmp/test")
	id, err := mgr.Send(ctx, pid, "init", "", nil)
	require.NoError(t, err)

	// Wait for RunDaemon to start
	waitForLoopStart(t, ch, id, 3*time.Second)
	waitForPendingInput(t, mgr.inboxStore, id, false, 3*time.Second)

	// Verify mock factory created a session and RunDaemon was called
	factory.mu.Lock()
	require.Len(t, factory.sessions, 1, "factory should have created one session")
	mockSess := factory.sessions[0]
	factory.mu.Unlock()

	mockSess.mu.Lock()
	ran := mockSess.ran
	mockSess.mu.Unlock()
	require.True(t, ran, "RunDaemon should have been called")

	require.NoError(t, mgr.SendToSession(ctx, id, "do something"))
	pending, err := mgr.inboxStore.PeekPending(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "do something", pending.RawContent)

	mgr.Shutdown(3 * time.Second)
}

func TestManager_SendDuplicateWorkdir(t *testing.T) {
	mgr, _, s := newTestManager(t)

	ctx := context.Background()
	pid := testProject(t, s, "/tmp/test")
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
	pid := testProject(t, s, "/tmp/test-input")
	id, err := mgr.Send(ctx, pid, "init", "", nil)
	require.NoError(t, err)

	sub := mgr.PubSub().Subscribe(id)
	defer mgr.PubSub().Unsubscribe(id, sub)

	mgr.pubsub.Publish(id, sessionevent.Notification{
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
	store sessionstore.InboxStore,
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
	pidA := testProject(t, s, "/tmp/a")
	_, err := mgr.Send(ctx, pidA, "init", "", nil)
	require.NoError(t, err)
	pidB := testProject(t, s, "/tmp/b")
	_, err = mgr.Send(ctx, pidB, "init", "", nil)
	require.NoError(t, err)

	mgr.Shutdown(5 * time.Second)

	mgr.mu.Lock()
	remaining := len(mgr.loops)
	mgr.mu.Unlock()
	assert.Zero(t, remaining, "all loops should be cleaned up after shutdown")
}

// ---------------------------------------------------------------------------
// Goroutine lifecycle tests
// ---------------------------------------------------------------------------

func TestManager_NormalCompletion(t *testing.T) {
	mgr, factory, s := newTestManager(t)
	ch := mgr.PubSub().SubscribeAll()

	sess := &mockSession{completeAfter: 50 * time.Millisecond}
	factory.nextSess = sess

	ctx := context.Background()
	pid := testProject(t, s, "/tmp/normal")
	id, err := mgr.Send(ctx, pid, "hi", "", nil)
	require.NoError(t, err)

	waitForState(t, ch, id, controllerapi.StateIdle, 3*time.Second)

	// Verify session removed from in-memory map
	assert.False(t, mgr.HasActiveLoop(id))
}

func TestManager_ErrorPath(t *testing.T) {
	mgr, factory, s := newTestManager(t)
	ch := mgr.PubSub().SubscribeAll()

	sess := &mockSession{
		completeAfter: 50 * time.Millisecond,
		runErr:        fmt.Errorf("something went wrong"),
	}
	factory.nextSess = sess

	ctx := context.Background()
	pid := testProject(t, s, "/tmp/error")
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
	ch := mgr.PubSub().SubscribeAll()

	// Session completes after 200ms — Kill sets killed flag, session finishes naturally
	sess := &mockSession{completeAfter: 200 * time.Millisecond}
	factory.nextSess = sess

	ctx := context.Background()
	pid := testProject(t, s, "/tmp/cancel")
	id, err := mgr.Send(ctx, pid, "init", "", nil)
	require.NoError(t, err)

	waitForLoopStart(t, ch, id, 3*time.Second)

	// Kill sets killed flag — does NOT cancel context
	err = mgr.Kill(context.Background(), id)
	require.NoError(t, err)

	waitForState(t, ch, id, controllerapi.StateIdle, 3*time.Second)
	assert.False(t, mgr.HasActiveLoop(id))
}

// ---------------------------------------------------------------------------
// Multi-session tests
// ---------------------------------------------------------------------------

func TestManager_SendCreatesNewSessionAfterCompletion(t *testing.T) {
	mgr, factory, s := newTestManager(t)
	ch := mgr.PubSub().SubscribeAll()

	// First session completes quickly
	factory.nextSess = &mockSession{completeAfter: 50 * time.Millisecond}

	ctx := context.Background()
	pid := testProject(t, s, "/tmp/resume")
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
	ch := mgr.PubSub().SubscribeAll()

	ctx := context.Background()
	pid := testProject(t, s, "/tmp/already-running")
	id, err := mgr.Send(ctx, pid, "init", "", nil)
	require.NoError(t, err)

	waitForLoopStart(t, ch, id, 3*time.Second)
	waitForPendingInput(t, mgr.inboxStore, id, false, 3*time.Second)

	// SendToSession on an already-running session — should route to inbox
	err = mgr.SendToSession(ctx, id, "steer message")
	require.NoError(t, err)

	pending, err := mgr.inboxStore.PeekPending(ctx, id)
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
	ch := mgr.PubSub().SubscribeAll()

	factory.nextSess = &mockSession{completeAfter: 50 * time.Millisecond}

	ctx := context.Background()
	pid := testProject(t, s, "/tmp/loop-restart")
	id1, err := mgr.Send(ctx, pid, "first", "", nil)
	require.NoError(t, err)

	waitForState(t, ch, id1, controllerapi.StateIdle, 3*time.Second)

	// Second Send creates a new session with its own loop
	factory.nextSess = &mockSession{completeAfter: 50 * time.Millisecond}
	id2, err := mgr.Send(ctx, pid, "second", "", nil)
	require.NoError(t, err)
	assert.NotEqual(t, id1, id2, "should create new session")

	waitForLoopStart(t, ch, id2, 3*time.Second)

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
	ch := mgr.PubSub().SubscribeAll()

	// First session completes quickly so Kill can work
	factory.nextSess = &mockSession{completeAfter: 100 * time.Millisecond}

	ctx := context.Background()
	pid := testProject(t, s, "/tmp/force")
	id1, err := mgr.Send(ctx, pid, "init", "", nil)
	require.NoError(t, err)

	_ = mgr.Kill(context.Background(), id1)
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
	ch := mgr.PubSub().SubscribeAll()

	// Session that blocks until context cancelled (Kill calls stop → cancel)
	factory.nextSess = &mockSession{}

	ctx := context.Background()
	pid := testProject(t, s, "/tmp/kill-graceful")
	id, err := mgr.Send(ctx, pid, "init", "", nil)
	require.NoError(t, err)

	waitForLoopStart(t, ch, id, 3*time.Second)

	// Kill is blocking: stop() + mark killed.
	err = mgr.Kill(context.Background(), id)
	require.NoError(t, err)

	waitForState(t, ch, id, controllerapi.StateIdle, 3*time.Second)

	// Session must be soft-deleted (killed_at set, but still in DB)
	rec, err := mgr.sessionStore.GetSession(context.Background(), id)
	require.NoError(t, err)
	assert.NotNil(t, rec.KilledAt, "killed session should have killed_at set")

	// Runner should be cleaned up
	assert.False(t, mgr.HasActiveLoop(id))
}

func TestManager_Kill_NonRunningSession(t *testing.T) {
	mgr, factory, s := newTestManager(t)
	ch := mgr.PubSub().SubscribeAll()

	factory.nextSess = &mockSession{completeAfter: 50 * time.Millisecond}

	ctx := context.Background()
	pid := testProject(t, s, "/tmp/kill-stopped")
	id, err := mgr.Send(ctx, pid, "init", "", nil)
	require.NoError(t, err)

	waitForState(t, ch, id, controllerapi.StateIdle, 3*time.Second)

	// Session is now idle (not in-memory). Kill should mark it killed.
	err = mgr.Kill(context.Background(), id)
	require.NoError(t, err)

	rec, err := mgr.sessionStore.GetSession(context.Background(), id)
	require.NoError(t, err)
	assert.NotNil(t, rec.KilledAt, "killed non-running session should have killed_at set")
}

// TestManager_Kill_RemovesSchedules: Kill owns schedule teardown — both one-shot
// and cron rows for the killed session are deleted, and other sessions' rows
// survive.
func TestManager_Kill_RemovesSchedules(t *testing.T) {
	mgr, factory, s, schedStore := newTestManagerWithSchedule(t)
	ch := mgr.PubSub().SubscribeAll()

	factory.nextSess = &mockSession{completeAfter: 50 * time.Millisecond}

	ctx := context.Background()
	pid := testProject(t, s, "/tmp/kill-schedules")
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
	require.NoError(t, mgr.Kill(context.Background(), id))

	remaining, err := schedStore.ListSchedules(ctx, id)
	require.NoError(t, err)
	assert.Empty(t, remaining, "both one-shot and cron schedules removed on kill")

	untouched, err := schedStore.ListSchedules(ctx, otherID)
	require.NoError(t, err)
	assert.Len(t, untouched, 1, "other session's schedule is untouched")
}

func TestManager_StopCancelsPendingSleepButPreservesScheduledWork(t *testing.T) {
	mgr, factory, projects, schedStore := newTestManagerWithSchedule(t)
	events := mgr.PubSub().SubscribeAll()

	factory.nextSess = &mockSession{completeAfter: 50 * time.Millisecond}
	ctx := context.Background()
	projectID := testProject(t, projects, "/tmp/stop-schedules")
	sessionID, err := mgr.Send(ctx, projectID, "init", "", nil)
	require.NoError(t, err)
	waitForState(t, events, sessionID, controllerapi.StateIdle, 3*time.Second)

	oneShot := time.Now().Add(time.Hour).UTC()
	_, err = schedStore.AddSchedule(ctx, sessionID, "", &oneShot, "scheduled work", false)
	require.NoError(t, err)
	_, err = schedStore.AddSchedule(ctx, sessionID, "0 9 * * *", nil, "recurring work", false)
	require.NoError(t, err)
	_, err = schedule.NewService(schedStore).AddSleep(
		ctx, sessionID, "sleep-call", time.Now().Add(2*time.Hour).UTC(), "wake",
	)
	require.NoError(t, err)

	require.NoError(t, mgr.Stop(ctx, sessionID, 0))

	pendingSleeps, err := schedule.NewService(schedStore).PendingSleeps(ctx, sessionID)
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
	ch := mgr.PubSub().SubscribeAll()

	// Session completes quickly → loop exits → session is idle
	factory.nextSess = &mockSession{completeAfter: 50 * time.Millisecond}

	ctx := context.Background()
	pid := testProject(t, s, "/tmp/setmodel-idle")
	id, err := mgr.Send(ctx, pid, "init", "old-model", nil)
	require.NoError(t, err)

	waitForState(t, ch, id, controllerapi.StateIdle, 3*time.Second)
	assert.False(t, mgr.HasActiveLoop(id), "session should not be running after idle")

	// SetModel on an idle session must succeed (DB-first pattern)
	err = mgr.SetModel(context.Background(), id, "new-model", "high")
	require.NoError(t, err)

	// Verify the model was persisted in SQLite
	rec, err := mgr.sessionStore.GetSession(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, "new-model", rec.Model)
	assert.Equal(t, "high", rec.ReasoningLevel)
}

func TestManager_SendToSession_RejectsKilledSession(t *testing.T) {
	mgr, factory, s := newTestManager(t)
	ch := mgr.PubSub().SubscribeAll()

	factory.nextSess = &mockSession{completeAfter: 50 * time.Millisecond}

	ctx := context.Background()
	pid := testProject(t, s, "/tmp/reject-killed")
	id, err := mgr.Send(ctx, pid, "init", "", nil)
	require.NoError(t, err)

	waitForState(t, ch, id, controllerapi.StateIdle, 3*time.Second)

	// Prepare a mock for the kill's resume path
	factory.mu.Lock()
	factory.nextSess = &mockSession{completeAfter: 50 * time.Millisecond}
	factory.mu.Unlock()

	err = mgr.Kill(context.Background(), id)
	require.NoError(t, err)

	// SendToSession on a killed session must return an error
	err = mgr.SendToSession(ctx, id, "should fail")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "killed")
}

func TestManager_Clear(t *testing.T) {
	mgr, factory, s := newTestManager(t)
	ch := mgr.PubSub().SubscribeAll()

	// Session completes quickly → loop exits → session is idle
	factory.nextSess = &mockSession{completeAfter: 50 * time.Millisecond}

	ctx := context.Background()
	pid := testProject(t, s, "/tmp/clear")
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
	newID, err := mgr.Clear(context.Background(), id)
	require.NoError(t, err)
	assert.NotEqual(t, id, newID, "new session should have a different ID")

	// New session should exist with same attributes, model, and reasoning level
	newRec, err := mgr.sessionStore.GetSession(context.Background(), newID)
	require.NoError(t, err)
	assert.Nil(t, newRec.KilledAt, "new session should not be killed")
	assert.Equal(t, "test-model", newRec.Model)
	assert.Equal(t, "high", newRec.ReasoningLevel)
	assert.Equal(t, "cli", newRec.Attributes["channel"])
	assert.InDelta(t, float64(42), newRec.Attributes["chat_id"], 0.0)
	assert.Equal(t, "cli", newRec.Attributes[controllerapi.SessionAttributeManagerID])

	// Old session should be killed (Kill ran synchronously inside Clear)
	oldRec, err := mgr.sessionStore.GetSession(context.Background(), id)
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
	pid := testProject(t, store, "/tmp/manager-owner-immutable")
	rec, err := mgr.sessionStore.CreateSession(ctx, pid, "test-model", "", map[string]any{
		controllerapi.SessionAttributeManagerID: "alpha",
	})
	require.NoError(t, err)

	require.NoError(t, mgr.SetAttributes(ctx, rec.ID, map[string]any{"topic": float64(42)}))
	stored, err := mgr.sessionStore.GetSession(ctx, rec.ID)
	require.NoError(t, err)
	assert.Equal(t, "alpha", stored.Attributes[controllerapi.SessionAttributeManagerID])
	assert.InDelta(t, float64(42), stored.Attributes["topic"], 0)

	err = mgr.SetAttributes(ctx, rec.ID, map[string]any{
		controllerapi.SessionAttributeManagerID: "beta",
	})
	require.ErrorContains(t, err, `belongs to manager "alpha"`)
	stored, err = mgr.sessionStore.GetSession(ctx, rec.ID)
	require.NoError(t, err)
	assert.Equal(t, "alpha", stored.Attributes[controllerapi.SessionAttributeManagerID])
}

func TestManager_ConcurrentManagerClaimsHaveExactlyOneWinner(t *testing.T) {
	mgr, _, store := newTestManager(t)
	ctx := context.Background()
	pid := testProject(t, store, "/tmp/concurrent-manager-claim")
	rec, err := mgr.sessionStore.CreateSession(ctx, pid, "test-model", "", nil)
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

	stored, err := mgr.sessionStore.GetSession(ctx, rec.ID)
	require.NoError(t, err)
	assert.Contains(t, []string{"alpha", "beta"}, stored.Attributes[controllerapi.SessionAttributeManagerID])
}

func TestManager_ClearRejectsAConcurrentLateOwnerClaim(t *testing.T) {
	mgr, _, store := newTestManager(t)
	ctx := context.Background()
	pid := testProject(t, store, "/tmp/clear-concurrent-manager-claim")
	rec, err := mgr.sessionStore.CreateSession(ctx, pid, "test-model", "", nil)
	require.NoError(t, err)

	blocking := &blockingCreateSessionStore{
		OrchestrationStore: mgr.sessionStore,
		entered:            make(chan struct{}), release: make(chan struct{}),
	}
	mgr.sessionStore = blocking
	clearResult := make(chan struct {
		id  int64
		err error
	}, 1)
	go func() {
		id, clearErr := mgr.Clear(ctx, rec.ID)
		clearResult <- struct {
			id  int64
			err error
		}{id: id, err: clearErr}
	}()
	requireSignal(t, blocking.entered)
	if mgr.routeMu.TryLock() {
		mgr.routeMu.Unlock()
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

	replacement, err := mgr.sessionStore.GetSession(ctx, cleared.id)
	require.NoError(t, err)
	assert.NotContains(t, replacement.Attributes, controllerapi.SessionAttributeManagerID)
}

func TestManager_ClearWhileRunning(t *testing.T) {
	mgr, factory, s := newTestManager(t)
	ch := mgr.PubSub().SubscribeAll()

	// Session that blocks until context cancelled
	factory.nextSess = &mockSession{}

	ctx := context.Background()
	pid := testProject(t, s, "/tmp/clear-running")
	id, err := mgr.Send(ctx, pid, "init", "my-model", map[string]any{"lang": "en"})
	require.NoError(t, err)

	waitForLoopStart(t, ch, id, 3*time.Second)

	// Clear: notifies immediately (via pubsub buffer), then kills old session synchronously
	newID, err := mgr.Clear(context.Background(), id)
	require.NoError(t, err)
	assert.NotEqual(t, id, newID)

	// New session available
	newRec, err := mgr.sessionStore.GetSession(context.Background(), newID)
	require.NoError(t, err)
	assert.Nil(t, newRec.KilledAt)
	assert.Equal(t, "my-model", newRec.Model)
	assert.Equal(t, "en", newRec.Attributes["lang"])

	// Runner stopped, old session killed
	assert.False(t, mgr.HasActiveLoop(id))
	oldRec, err := mgr.sessionStore.GetSession(context.Background(), id)
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
	ch := mgr.PubSub().SubscribeAll()

	factory.nextSess = &mockSession{completeAfter: 50 * time.Millisecond}

	ctx := context.Background()
	pid := testProject(t, s, "/tmp/clear-restart")
	id, err := mgr.Send(ctx, pid, "init", "", nil)
	require.NoError(t, err)

	waitForState(t, ch, id, controllerapi.StateIdle, 3*time.Second)

	// Simulate: Clear set terminating but daemon died before Kill completed
	require.NoError(t, mgr.sessionStore.UpdateSessionStatus(
		context.Background(), id, sessionstore.SessionStatusTerminating,
	))

	// Simulate daemon restart — New() calls KillTerminatingSessions
	require.NoError(t, mgr.sessionStore.KillTerminatingSessions(context.Background()))

	rec, err := mgr.sessionStore.GetSession(context.Background(), id)
	require.NoError(t, err)
	assert.NotNil(t, rec.KilledAt, "terminating session should be killed on startup")
}

// ---------------------------------------------------------------------------
// Control-plane tool gating
// ---------------------------------------------------------------------------

// TestRegisterControlPlaneTools_RespectsAgentTypeAllowlist: dynamic tools are
// re-checked against the session allowlist, and schedule has the additional
// root-session boundary.
func TestRegisterControlPlaneTools_RespectsAgentTypeAllowlist(t *testing.T) {
	mgr, _, _, _ := newTestManagerWithSchedule(t)
	ctx := context.Background()

	gatedIDs := []string{"schedule", "sleep", "task", "get_subagent_result", "send_to_subagent"}

	explore := &mockSession{agentType: registry.AgentTypeExplore}
	mgr.registerScheduleTools(ctx, &sessionstore.SessionRecord{ID: 1, ParentID: 10}, explore)
	mgr.registerSubagentTools(ctx, 1, explore)

	for _, id := range gatedIDs {
		assert.False(t, explore.hasTool(id), "explore session must not gain %q", id)
	}

	build := &mockSession{agentType: registry.AgentTypeBuild}
	mgr.registerScheduleTools(ctx, &sessionstore.SessionRecord{ID: 2}, build)
	mgr.registerSubagentTools(ctx, 2, build)

	for _, id := range gatedIDs {
		assert.True(t, build.hasTool(id), "build session must keep %q", id)
	}

	general := &mockSession{agentType: registry.AgentTypeGeneral}
	mgr.registerScheduleTools(ctx, &sessionstore.SessionRecord{ID: 3, ParentID: 2}, general)
	assert.False(t, general.hasTool(tool.IDSchedule))
	assert.True(t, general.hasTool(tool.IDSleep))
}

func TestDeliverScheduleToSubagentDoesNotConstructSession(t *testing.T) {
	tests := []struct {
		name    string
		deliver func(*svc, context.Context, int64) (bool, error)
	}{
		{
			name: "normal",
			deliver: func(mgr *svc, ctx context.Context, sessionID int64) (bool, error) {
				return mgr.DeliverScheduleTick(ctx, sessionID, "schedule:legacy:normal", "legacy task")
			},
		},
		{
			name: "fresh",
			deliver: func(mgr *svc, ctx context.Context, sessionID int64) (bool, error) {
				return mgr.DeliverFreshSchedule(ctx, sessionID, "schedule:legacy:fresh", "legacy task")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr, factory, projects, _ := newTestManagerWithSchedule(t)
			ctx := context.Background()
			projectID := testProject(t, projects, t.TempDir())

			parent, err := mgr.sessionStore.CreateSession(ctx, projectID, "fake-model", "", nil)
			require.NoError(t, err)
			childID, err := mgr.sessionStore.CreateSubagentSession(
				ctx, projectID, parent.ID, parent.ID, "general", "fake-model", "",
			)
			require.NoError(t, err)
			require.NoError(t, mgr.sessionStore.UpdateSessionStatus(ctx, childID, sessionstore.SessionStatusCompleted))

			applied, err := tt.deliver(mgr, ctx, childID)
			require.NoError(t, err)
			assert.False(t, applied)

			factory.mu.Lock()
			defer factory.mu.Unlock()
			assert.Empty(t, factory.sessions, "legacy subagent delivery must not construct a session")
		})
	}
}

func TestEnsureRunner_EmptyRootPublishesCreatedAndIdle(t *testing.T) {
	mgr, _, projects := newTestManager(t)
	ctx := context.Background()
	workDir := t.TempDir()
	projectID := testProject(t, projects, workDir)
	rec, err := mgr.sessionStore.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)
	notifications := mgr.PubSub().SubscribeAll()
	defer mgr.PubSub().UnsubscribeAll(notifications)

	require.NoError(t, mgr.ensureRunner(ctx, rec.ID, workDir, projectID, nil))

	created := requireNotification(t, notifications)
	assert.Equal(t, sessionevent.NotifySessionCreated, created.Notification.Type)
	idle := requireNotification(t, notifications)
	assert.Equal(t, sessionevent.NotifyStateChanged, idle.Notification.Type)
	assert.Equal(t, controllerapi.StateIdle, idle.Notification.Status)
	require.Eventually(t, func() bool { return !mgr.HasActiveLoop(rec.ID) }, time.Second, 10*time.Millisecond)
}
