package daemon

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/admission"
	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

type claimFailStore struct {
	backgroundprocess.Store
	mu        sync.Mutex
	remaining int
}

type markDeliveredFailStore struct {
	backgroundprocess.Store
	mu        sync.Mutex
	remaining int
}

type claimBarrierStore struct {
	backgroundprocess.Store
	mu      sync.Mutex
	calls   int
	entered chan struct{}
	release chan struct{}
}

func (s *claimBarrierStore) ClaimDelivery(
	ctx context.Context,
	id string,
) (int64, bool, error) {
	s.mu.Lock()
	s.calls++
	blocked := s.calls == 1
	if blocked {
		close(s.entered)
	}
	s.mu.Unlock()

	if blocked {
		select {
		case <-s.release:
		case <-ctx.Done():
			return 0, false, ctx.Err()
		}
	}

	return s.Store.ClaimDelivery(ctx, id)
}

func (s *markDeliveredFailStore) MarkDelivered(
	ctx context.Context,
	id string,
) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.remaining > 0 {
		s.remaining--

		return false, errors.New("injected delivery acknowledgement failure")
	}

	return s.Store.MarkDelivered(ctx, id)
}

func (s *claimFailStore) ClaimDelivery(
	ctx context.Context,
	id string,
) (int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.remaining > 0 {
		s.remaining--

		return 0, false, errors.New("injected delivery claim failure")
	}

	return s.Store.ClaimDelivery(ctx, id)
}

type processRouteFailSink struct {
	processDeliverySink
	mu              sync.Mutex
	fenceFailures   int
	enqueueFailures int
	blockFence      bool
	fenceEntered    chan struct{}
	enterOnce       sync.Once
}

func (s *processRouteFailSink) withProcessDeliveryFence(
	ctx context.Context,
	rootID int64,
	run func() error,
) error {
	s.mu.Lock()
	if s.fenceFailures > 0 {
		s.fenceFailures--
		s.mu.Unlock()

		return errors.New("injected delivery fence failure")
	}
	s.mu.Unlock()
	if s.blockFence {
		s.enterOnce.Do(func() { close(s.fenceEntered) })
		<-ctx.Done()

		return ctx.Err()
	}

	return s.processDeliverySink.withProcessDeliveryFence(ctx, rootID, run)
}

func (s *processRouteFailSink) enqueueProcessCompletionLocked(
	ctx context.Context,
	target int64,
	input processCompletionInput,
) error {
	s.mu.Lock()
	if s.enqueueFailures > 0 {
		s.enqueueFailures--
		s.mu.Unlock()

		return errors.New("injected delivery enqueue failure")
	}
	s.mu.Unlock()

	return s.processDeliverySink.enqueueProcessCompletionLocked(ctx, target, input)
}

func TestScenario_DirectChildLifecycleCancelsOnlyItsProcessSubtree(t *testing.T) {
	tests := []struct {
		name string
		run  func(context.Context, *svc, int64) error
	}{
		{name: "stop", run: func(ctx context.Context, manager *svc, id int64) error {
			return manager.Stop(ctx, id, 0)
		}},
		{name: "kill", run: func(ctx context.Context, manager *svc, id int64) error {
			return manager.Kill(ctx, id)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newSubagentHarnessWith(t, trivialRespond)
			service := installScenarioProcessService(t, h)
			defer h.shutdown()

			root, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
			require.NoError(t, err)
			childID := createBackgroundChild(t, h.mgr, h.projectID, root.ID)
			siblingID := createBackgroundChild(t, h.mgr, h.projectID, root.ID)
			child := startScenarioProcess(t, service, childID, root.ID, "sleep 30")
			sibling := startScenarioProcess(t, service, siblingID, root.ID, "sleep 30")

			require.NoError(t, tt.run(h.ctx, h.mgr, childID))
			assert.Equal(t, backgroundprocess.StateCancelled,
				waitScenarioProcessState(t, h, child.ID, backgroundprocess.StateCancelled).State)

			siblingRecord, err := h.mgr.processStore.GetProcess(h.ctx, sibling.ID)
			require.NoError(t, err)
			assert.Equal(t, backgroundprocess.StateRunning, siblingRecord.State)

			_, err = service.CancelAll(h.ctx, backgroundprocess.IntentDaemonShutdown)
			require.NoError(t, err)
		})
	}
}

func TestScenario_ProcessCompletionWaitsForRunnerCapacity(t *testing.T) {
	tests := []struct {
		name       string
		makeTarget func(*testing.T, *subagentHarness, int64) int64
		queued     func(*svc) bool
		drain      func(*svc)
	}{
		{
			name: "root",
			makeTarget: func(_ *testing.T, _ *subagentHarness, rootID int64) int64 {
				return rootID
			},
			queued: func(manager *svc) bool { return manager.pendingQueue.Len() == 1 },
			drain:  func(manager *svc) { manager.drainPendingRunners(context.Background()) },
		},
		{
			name: "nonblocking child",
			makeTarget: func(t *testing.T, h *subagentHarness, rootID int64) int64 {
				childID := createBackgroundChild(t, h.mgr, h.projectID, rootID)
				require.NoError(t, h.sessStore.UpdateSessionStatus(
					h.ctx, childID, sessionstore.SessionStatusSuspended,
				))

				return childID
			},
			queued: func(manager *svc) bool { return manager.childQueue.Len() == 1 },
			drain:  func(manager *svc) { manager.drainQueue(context.Background()) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newSubagentHarnessWith(t, func(_ string, messages []llmwire.Message) *llmwire.Response {
				if hasToolResultFor(messages, "process_event") {
					return &llmwire.Response{Text: "capacity-delayed process completion"}
				}

				return &llmwire.Response{Text: "unexpected activation"}
			})
			service := installScenarioProcessService(t, h)
			defer h.shutdown()

			root, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
			require.NoError(t, err)
			require.NoError(t, h.sessStore.UpdateSessionStatus(
				h.ctx, root.ID, sessionstore.SessionStatusCompleted,
			))
			target := tt.makeTarget(t, h, root.ID)

			for range admission.MaxTotal {
				require.True(t, h.mgr.admit.TryAdmit(admission.Parent, 0))
			}
			held := admission.MaxTotal
			defer func() {
				for range held {
					h.mgr.admit.Release(admission.Parent, 0)
				}
			}()

			process := startScenarioProcess(t, service, target, root.ID, "printf done")
			require.Eventually(t, func() bool {
				record, loadErr := h.mgr.processStore.GetProcess(h.ctx, process.ID)

				return loadErr == nil && record.DeliveryState == "claimed" && tt.queued(h.mgr)
			}, 5*time.Second, 10*time.Millisecond)
			assert.Equal(t, 0, processEventCount(t, h, target))

			h.mgr.admit.Release(admission.Parent, 0)
			held--
			tt.drain(h.mgr)

			require.Eventually(t, func() bool {
				record, loadErr := h.mgr.processStore.GetProcess(h.ctx, process.ID)

				return loadErr == nil && record.DeliveryState == "delivered" &&
					processEventCount(t, h, target) == 1
			}, 5*time.Second, 10*time.Millisecond)
		})
	}
}

func TestScenario_StatusScopesBackgroundProcessesWithoutCommandText(t *testing.T) {
	h := newSubagentHarnessWith(t, trivialRespond)
	service := installScenarioProcessService(t, h)
	defer h.shutdown()

	root, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)
	childID := createBackgroundChild(t, h.mgr, h.projectID, root.ID)
	otherRoot, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)

	rootProcess := startScenarioProcess(t, service, root.ID, root.ID, "sleep 30")
	childProcess := startScenarioProcess(t, service, childID, root.ID, "sleep 30")
	otherProcess := startScenarioProcess(t, service, otherRoot.ID, otherRoot.ID, "sleep 30")

	current, err := h.mgr.CurrentProgress(h.ctx, root.ID)
	require.NoError(t, err)
	assert.Contains(t, current.Rendered, "Background processes: 2 running")
	assert.Contains(t, current.Rendered, rootProcess.ID)
	assert.Contains(t, current.Rendered, childProcess.ID)
	assert.Contains(t, current.Rendered, "owner main")
	assert.Contains(t, current.Rendered, "owner subagent "+strconv.FormatInt(childID, 10))
	assert.NotContains(t, current.Rendered, otherProcess.ID)
	assert.NotContains(t, current.Rendered, "sleep 30")

	_, err = service.CancelAll(h.ctx, backgroundprocess.IntentDaemonShutdown)
	require.NoError(t, err)
}

func TestScenario_ClaimedProcessRetriesRunnerFailures(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*mockFactory)
	}{
		{name: "session setup", configure: func(factory *mockFactory) {
			factory.createErrOnce = errors.New("injected session setup failure")
		}},
		{name: "process injection", configure: func(factory *mockFactory) {
			factory.nextSess = &mockSession{injectErr: errors.New("injected process injection failure")}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			manager, factory, projects := newTestManager(t)
			defer manager.Shutdown(3 * time.Second)
			projectID := testProject(t, projects, "/tmp/process-retry-"+tt.name)
			root, err := manager.sessionStore.CreateSession(ctx, projectID, "fake-model", "", nil)
			require.NoError(t, err)
			require.NoError(t, manager.sessionStore.UpdateSessionStatus(
				ctx, root.ID, sessionstore.SessionStatusCompleted,
			))
			tt.configure(factory)

			now := time.Now().UTC()
			record := backgroundprocess.Process{
				ID:        "retry-" + strings.ReplaceAll(tt.name, " ", "-"),
				SessionID: root.ID, RootSessionID: root.ID, ToolCallID: "process-call",
				OutputPath: "/tmp/missing-process-output", Deadline: now.Add(time.Minute),
				CreatedAt: now, AdvertisedAt: &now, State: backgroundprocess.StateRunning,
			}
			require.NoError(t, manager.processStore.InsertProcess(ctx, record))
			zero := 0
			final, won, err := manager.processStore.Finalize(
				ctx, record.ID, backgroundprocess.StateCompleted, &zero, 0,
			)
			require.NoError(t, err)
			require.True(t, won)

			_, _, err = manager.processCoord.Route(ctx, restartCompletion(final))
			require.NoError(t, err)
			require.Eventually(t, func() bool {
				current, loadErr := manager.processStore.GetProcess(ctx, record.ID)

				return loadErr == nil && current.DeliveryState == "delivered"
			}, 5*time.Second, 10*time.Millisecond)
		})
	}
}

func TestScenario_ProcessCompletionRetriesRoutingFailures(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*svc)
	}{
		{name: "claim", configure: func(manager *svc) {
			store := &claimFailStore{Store: manager.processStore, remaining: 1}
			manager.processStore = store
			manager.processCoord = newProcessCoordinator(store, manager)
		}},
		{name: "fence", configure: func(manager *svc) {
			sink := &processRouteFailSink{processDeliverySink: manager, fenceFailures: 1}
			manager.processCoord = newProcessCoordinator(manager.processStore, sink)
		}},
		{name: "enqueue", configure: func(manager *svc) {
			sink := &processRouteFailSink{processDeliverySink: manager, enqueueFailures: 1}
			manager.processCoord = newProcessCoordinator(manager.processStore, sink)
		}},
		{name: "acknowledgement", configure: func(manager *svc) {
			store := &markDeliveredFailStore{Store: manager.processStore, remaining: 1}
			manager.processStore = store
			manager.processCoord = newProcessCoordinator(store, manager)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			manager, _, projects := newTestManager(t)
			defer manager.Shutdown(3 * time.Second)
			projectID := testProject(t, projects, "/tmp/process-route-retry-"+tt.name)
			root, err := manager.sessionStore.CreateSession(ctx, projectID, "fake-model", "", nil)
			require.NoError(t, err)
			require.NoError(t, manager.sessionStore.UpdateSessionStatus(
				ctx, root.ID, sessionstore.SessionStatusCompleted,
			))
			tt.configure(manager)

			now := time.Now().UTC()
			record := backgroundprocess.Process{
				ID: "route-retry-" + tt.name, SessionID: root.ID, RootSessionID: root.ID,
				ToolCallID: "process-call", OutputPath: "/tmp/missing-process-output",
				Deadline: now.Add(time.Minute), CreatedAt: now, AdvertisedAt: &now,
				State: backgroundprocess.StateRunning,
			}
			require.NoError(t, manager.processStore.InsertProcess(ctx, record))
			zero := 0
			final, won, err := manager.processStore.Finalize(
				ctx, record.ID, backgroundprocess.StateCompleted, &zero, 0,
			)
			require.NoError(t, err)
			require.True(t, won)

			manager.routeProcessCompletion(ctx, restartCompletion(final))
			require.Eventually(t, func() bool {
				current, loadErr := manager.processStore.GetProcess(ctx, record.ID)

				return loadErr == nil && current.DeliveryState == "delivered"
			}, 5*time.Second, 10*time.Millisecond)
		})
	}
}

func TestScenario_ProcessRetryWorkerJoinsShutdown(t *testing.T) {
	ctx := context.Background()
	manager, _, projects := newTestManager(t)
	projectID := testProject(t, projects, "/tmp/process-retry-shutdown")
	root, err := manager.sessionStore.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)
	require.NoError(t, manager.sessionStore.UpdateSessionStatus(
		ctx, root.ID, sessionstore.SessionStatusCompleted,
	))

	sink := &processRouteFailSink{
		processDeliverySink: manager,
		fenceFailures:       1,
		blockFence:          true,
		fenceEntered:        make(chan struct{}),
	}
	manager.processCoord = newProcessCoordinator(manager.processStore, sink)
	now := time.Now().UTC()
	record := backgroundprocess.Process{
		ID: "retry-shutdown", SessionID: root.ID, RootSessionID: root.ID,
		ToolCallID: "process-call", OutputPath: "/tmp/missing-process-output",
		Deadline: now.Add(time.Minute), CreatedAt: now, AdvertisedAt: &now,
		State: backgroundprocess.StateRunning,
	}
	require.NoError(t, manager.processStore.InsertProcess(ctx, record))
	zero := 0
	final, won, err := manager.processStore.Finalize(
		ctx, record.ID, backgroundprocess.StateCompleted, &zero, 0,
	)
	require.NoError(t, err)
	require.True(t, won)

	manager.routeProcessCompletion(ctx, restartCompletion(final))
	select {
	case <-sink.fenceEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("retry worker did not enter the blocked fence")
	}

	done := make(chan struct{})
	go func() {
		manager.Shutdown(2 * time.Second)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("daemon shutdown did not join the retry worker")
	}
}

func TestScenario_ProcessRetryKeepsOwnershipAcrossConcurrentClaim(t *testing.T) {
	h := newSubagentHarnessWith(t, trivialRespond)
	defer h.shutdown()

	root, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)
	require.NoError(t, h.sessStore.UpdateSessionStatus(
		h.ctx, root.ID, sessionstore.SessionStatusCompleted,
	))

	base := h.mgr.processStore
	store := &claimBarrierStore{
		Store: base, entered: make(chan struct{}), release: make(chan struct{}),
	}
	sink := &processRouteFailSink{
		processDeliverySink: h.mgr, fenceFailures: 1,
	}
	h.mgr.processStore = store
	h.mgr.processCoord = newProcessCoordinator(store, sink)

	now := time.Now().UTC()
	record := backgroundprocess.Process{
		ID: "retry-concurrent-claim", SessionID: root.ID, RootSessionID: root.ID,
		ToolCallID: "process-call", OutputPath: "/tmp/missing-process-output",
		Deadline: now.Add(time.Minute), CreatedAt: now, AdvertisedAt: &now,
		State: backgroundprocess.StateRunning,
	}
	require.NoError(t, store.InsertProcess(h.ctx, record))
	zero := 0
	final, won, err := store.Finalize(
		h.ctx, record.ID, backgroundprocess.StateCompleted, &zero, 0,
	)
	require.NoError(t, err)
	require.True(t, won)

	started := time.Now()
	h.mgr.routeProcessCompletion(h.ctx, restartCompletion(final))
	select {
	case <-store.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("pending retry did not reach the claim barrier")
	}

	target, claimed, err := base.ClaimDelivery(h.ctx, record.ID)
	require.NoError(t, err)
	require.True(t, claimed)
	require.Equal(t, root.ID, target)
	close(store.release)

	require.Eventually(t, func() bool {
		current, loadErr := store.GetProcess(h.ctx, record.ID)

		return loadErr == nil && current.DeliveryState == "delivered" &&
			processEventCount(t, h, root.ID) == 1
	}, 5*time.Second, 10*time.Millisecond)
	assert.Less(t, time.Since(started), processRetryDelay<<(processRetryMaxExponent+1))

	require.Eventually(t, func() bool {
		h.mgr.processRetryMu.Lock()
		_, retrying := h.mgr.processRetries[record.ID]
		h.mgr.processRetryMu.Unlock()

		return !retrying
	}, 2*time.Second, 10*time.Millisecond)

	joined := make(chan struct{})
	go func() {
		h.mgr.workerWG.Wait()
		close(joined)
	}()
	select {
	case <-joined:
	case <-time.After(2 * time.Second):
		t.Fatal("retry worker remained after durable acknowledgement")
	}
	assert.Equal(t, 1, processEventCount(t, h, root.ID))
}
