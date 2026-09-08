package backgroundprocess

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordIntentFailStore struct {
	Store
	failID string
}

type blockingFinalizeStore struct {
	Store
	entered chan struct{}
	release chan struct{}
}

func (s *blockingFinalizeStore) Finalize(
	ctx context.Context,
	id string,
	natural State,
	exitCode *int,
	outputSize int64,
) (Process, bool, error) {
	close(s.entered)
	<-s.release

	return s.Store.Finalize(ctx, id, natural, exitCode, outputSize)
}

type listRunningFailStore struct {
	Store
}

func (listRunningFailStore) ListRunning(context.Context) ([]Process, error) {
	return nil, errors.New("injected list failure")
}

type getProcessFailStore struct {
	Store
}

type listTreeFailStore struct {
	Store
}

func (listTreeFailStore) ListRunningByRoot(context.Context, int64) ([]Process, error) {
	return nil, errors.New("injected tree list failure")
}

type listSessionsFailStore struct {
	Store
}

func (listSessionsFailStore) ListRunningBySessions(context.Context, []int64) ([]Process, error) {
	return nil, errors.New("injected session list failure")
}

func (getProcessFailStore) GetProcess(context.Context, string) (Process, error) {
	return Process{}, errors.New("injected get failure")
}

func (s *recordIntentFailStore) RecordIntent(
	ctx context.Context,
	id string,
	intent HostIntent,
) (bool, error) {
	if id == s.failID {
		return false, errors.New("injected intent failure")
	}

	return s.Store.RecordIntent(ctx, id, intent)
}

func startTestProcess(t *testing.T, service Service, spec Spec, command string) Process {
	t.Helper()

	record, err := service.Start(context.Background(), spec, func(ctx context.Context) (*exec.Cmd, error) {
		return exec.CommandContext(ctx, "sh", "-c", command), nil
	})
	require.NoError(t, err)

	return record
}

func TestStore_RejectsOwnerRootMismatch(t *testing.T) {
	store := newTestStore(t)
	record := runningRecord(t, store, 2)
	record.ID = newTestID()
	record.RootSessionID = 2

	err := store.InsertProcess(context.Background(), record)
	require.ErrorContains(t, err, "owner/root mismatch")
}

func TestService_CancelSessionsDoesNotCancelSiblingOwner(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	service := newTestService(t, store, nil, nil)

	child := startTestProcess(t, service, testSpec(2), "sleep 30")
	sibling := startTestProcess(t, service, testSpec(3), "sleep 30")

	cancelled, err := service.CancelSessions(ctx, []int64{2}, IntentSessionStopped)
	require.NoError(t, err)
	assert.Equal(t, 1, cancelled)
	assert.Equal(t, StateCancelled, waitState(t, store, child.ID, StateCancelled, 5*time.Second).State)

	siblingRecord, err := store.GetProcess(ctx, sibling.ID)
	require.NoError(t, err)
	assert.Equal(t, StateRunning, siblingRecord.State)

	_, err = service.CancelAll(ctx, IntentDaemonShutdown)
	require.NoError(t, err)
}

func TestService_CancelAllJoinsEveryHandleAfterIntentFailure(t *testing.T) {
	ctx := context.Background()
	base := newTestStore(t)
	store := &recordIntentFailStore{Store: base}
	service := newTestService(t, store, nil, nil)

	first := startTestProcess(t, service, testSpec(2), "sleep 30")
	second := startTestProcess(t, service, testSpec(3), "sleep 30")
	store.failID = first.ID

	cancelled, err := service.CancelAll(ctx, IntentDaemonShutdown)
	require.ErrorContains(t, err, "injected intent failure")
	assert.Equal(t, 1, cancelled)
	assert.Zero(t, service.liveCount(2))
	assert.Zero(t, service.liveCount(3))
	failedIntent := waitState(t, base, first.ID, StateInterrupted, 5*time.Second)
	assert.Equal(t, "pending", failedIntent.DeliveryState)
	assert.Equal(t, StateInterrupted, waitState(
		t, base, second.ID, StateInterrupted, 5*time.Second,
	).State)
}

func TestService_FallbackIntentPreservesCancellationOutcome(t *testing.T) {
	tests := []struct {
		intent        HostIntent
		state         State
		deliveryState string
	}{
		{intent: IntentSessionStopped, state: StateCancelled, deliveryState: "suppressed"},
		{intent: IntentSessionKilled, state: StateCancelled, deliveryState: "suppressed"},
		{intent: IntentDaemonShutdown, state: StateInterrupted, deliveryState: "pending"},
	}

	for _, tt := range tests {
		t.Run(string(tt.intent), func(t *testing.T) {
			ctx := context.Background()
			base := newTestStore(t)
			store := &recordIntentFailStore{Store: base}
			service := newTestService(t, store, nil, nil)
			process := startTestProcess(t, service, testSpec(2), "sleep 30")
			store.failID = process.ID

			cancelled, err := service.CancelProcess(ctx, process.ID, tt.intent)
			require.ErrorContains(t, err, "injected intent failure")
			assert.Zero(t, cancelled)

			final := waitState(t, base, process.ID, tt.state, 5*time.Second)
			assert.Equal(t, tt.deliveryState, final.DeliveryState)
			assert.Equal(t, tt.intent, final.HostIntent)

			count, err := base.CountTerminalByIntentSince(ctx, 1, tt.intent, time.Time{})
			require.NoError(t, err)
			assert.Equal(t, 1, count)
		})
	}
}

func TestService_FinalizationExcludesLateFallbackIntent(t *testing.T) {
	ctx := context.Background()
	base := newTestStore(t)
	blocked := &blockingFinalizeStore{
		Store: base, entered: make(chan struct{}), release: make(chan struct{}),
	}
	store := &recordIntentFailStore{Store: blocked}
	service := newTestService(t, store, nil, nil)
	process := startTestProcess(t, service, testSpec(2), "true")
	store.failID = process.ID
	<-blocked.entered

	done := make(chan error, 1)
	go func() {
		_, err := service.CancelProcess(ctx, process.ID, IntentSessionStopped)
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("cancellation crossed an in-flight finalization: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(blocked.release)
	require.ErrorContains(t, <-done, "injected intent failure")
	final := waitState(t, base, process.ID, StateCompleted, 5*time.Second)
	assert.Equal(t, IntentNone, final.HostIntent)
	assert.Zero(t, service.liveCount(2))
}

func TestService_CancelAllListFailureStillInterruptsEveryHandle(t *testing.T) {
	ctx := context.Background()
	base := newTestStore(t)
	store := listRunningFailStore{Store: base}
	service := newTestService(t, store, nil, nil)
	first := startTestProcess(t, service, testSpec(2), "sleep 30")
	second := startTestProcess(t, service, testSpec(3), "sleep 30")

	cancelled, err := service.CancelAll(ctx, IntentDaemonShutdown)
	require.ErrorContains(t, err, "injected list failure")
	assert.Equal(t, 2, cancelled)

	for _, process := range []Process{first, second} {
		final := waitState(t, base, process.ID, StateInterrupted, 5*time.Second)
		assert.Equal(t, "pending", final.DeliveryState)
		assert.Equal(t, IntentDaemonShutdown, final.HostIntent)
	}
	assert.Zero(t, service.liveCount(2))
	assert.Zero(t, service.liveCount(3))
}

func TestService_CancelProcessReadFailurePreservesIntent(t *testing.T) {
	ctx := context.Background()
	base := newTestStore(t)
	store := getProcessFailStore{Store: base}
	service := newTestService(t, store, nil, nil)
	process := startTestProcess(t, service, testSpec(2), "sleep 30")

	cancelled, err := service.CancelProcess(ctx, process.ID, IntentSessionStopped)
	require.ErrorContains(t, err, "injected get failure")
	assert.Zero(t, cancelled)

	final := waitState(t, base, process.ID, StateCancelled, 5*time.Second)
	assert.Equal(t, IntentSessionStopped, final.HostIntent)
	assert.Equal(t, "suppressed", final.DeliveryState)
}

func TestService_DetachedCancellationFailurePersistsIntent(t *testing.T) {
	ctx := context.Background()
	base := newTestStore(t)
	process := runningRecord(t, base, 2)
	store := &recordIntentFailStore{Store: base, failID: process.ID}
	service := newTestService(t, store, nil, nil)

	cancelled, err := service.CancelSessions(ctx, []int64{2}, IntentSessionKilled)
	require.ErrorContains(t, err, "injected intent failure")
	assert.Zero(t, cancelled)

	final := waitState(t, base, process.ID, StateCancelled, 5*time.Second)
	assert.Equal(t, IntentSessionKilled, final.HostIntent)
	assert.Equal(t, "suppressed", final.DeliveryState)
}

func TestService_ScopedListFailureCancelsMatchingHandles(t *testing.T) {
	tests := []struct {
		name   string
		wrap   func(Store) Store
		cancel func(context.Context, Service) (int, error)
	}{
		{
			name: "tree",
			wrap: func(store Store) Store { return listTreeFailStore{Store: store} },
			cancel: func(ctx context.Context, service Service) (int, error) {
				return service.CancelTree(ctx, 1, IntentSessionStopped)
			},
		},
		{
			name: "sessions",
			wrap: func(store Store) Store { return listSessionsFailStore{Store: store} },
			cancel: func(ctx context.Context, service Service) (int, error) {
				return service.CancelSessions(ctx, []int64{2}, IntentSessionStopped)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			base := newTestStore(t)
			service := newTestService(t, tt.wrap(base), nil, nil)
			matched := startTestProcess(t, service, testSpec(2), "sleep 30")
			sibling := startTestProcess(t, service, testSpec(3), "sleep 30")
			if tt.name == "tree" {
				sibling = startTestProcess(t, service, Spec{
					SessionID: 4, RootSessionID: 4, ToolCallID: "outside", Deadline: time.Minute,
				}, "sleep 30")
			}

			cancelled, err := tt.cancel(ctx, service)
			require.ErrorContains(t, err, "list failure")
			assert.Positive(t, cancelled)
			final := waitState(t, base, matched.ID, StateCancelled, 5*time.Second)
			assert.Equal(t, IntentSessionStopped, final.HostIntent)

			siblingRecord, err := base.GetProcess(ctx, sibling.ID)
			require.NoError(t, err)
			assert.Equal(t, StateRunning, siblingRecord.State)
			_, _ = service.CancelAll(ctx, IntentDaemonShutdown)
		})
	}
}
