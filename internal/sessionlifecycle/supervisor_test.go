package sessionlifecycle

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/admission"
	"github.com/pilat/coagent/internal/sessionstore"
)

type blockedAppendRunner struct {
	Runner[int]
	entered chan struct{}
	release chan struct{}
}

type stopSessions struct{ launcherSessions }

type recordingStopper struct {
	Stopper
	phases *[]string
}

type registeringSessions struct {
	launcherSessions
	entered chan struct{}
	release chan struct{}
}

func TestSupervisorRejectsInvalidTreeLock(t *testing.T) {
	s := &supervisor[int]{
		sessions: launcherSessions{record: &sessionstore.SessionRecord{ID: 1}},
	}
	s.locks.Store(int64(1), "invalid")

	unlock, err := s.LockTree(t.Context(), 1)
	require.ErrorContains(t, err, "invalid session tree lock for root 1")
	require.Nil(t, unlock)
}

func TestSupervisorRegistrationLosesToShutdown(t *testing.T) {
	sessions := registeringSessions{
		record:  &sessionstore.SessionRecord{ID: 1},
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	var runs atomic.Int64
	s := NewSupervisor(sessions, launcherLinks{},
		func(context.Context, *sessionstore.SessionRecord, []int) (bool, error) { return false, nil },
		nil, func(context.Context, int64, Runner[int]) { runs.Add(1) },
	)
	result := make(chan error, 1)
	go func() { result <- s.EnsureLocked(t.Context(), 1, "", 0, []int{1}) }()
	<-sessions.entered
	s.Shutdown(func() {}, func() {})
	close(sessions.release)
	require.ErrorIs(t, <-result, ErrShuttingDown)
	assert.Zero(t, runs.Load())
	assert.Zero(t, s.Count())
	assert.Zero(t, s.LiveTotal())
}

func TestSupervisorDuplicateEnsureKeepsSingleSlot(t *testing.T) {
	started := make(chan Runner[int], 1)
	s := NewSupervisor(launcherSessions{record: &sessionstore.SessionRecord{ID: 1}}, launcherLinks{},
		func(context.Context, *sessionstore.SessionRecord, []int) (bool, error) { return false, nil },
		nil, func(_ context.Context, _ int64, r Runner[int]) { started <- r },
	)
	require.NoError(t, s.Ensure(t.Context(), 1, "", 0, []int{1}))
	<-started
	require.NoError(t, s.Ensure(t.Context(), 1, "", 0, []int{2}))
	assert.Equal(t, int64(1), s.LiveTotal())
	assert.Equal(t, []int{1, 2}, s.Finish(1))
	assert.Zero(t, s.LiveTotal())
	s.Shutdown(func() {}, func() {})
}

func TestSupervisorAppendSerializesWithFinish(t *testing.T) {
	s := NewSupervisor[int](nil, nil, nil, nil, nil)
	r := &blockedAppendRunner{
		Runner:  NewRunner[int](func() {}, "", 0, admission.Parent, 0, false, nil),
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	require.True(t, s.Attach(1, r))
	appended := make(chan bool, 1)
	go func() { appended <- s.Append(1, 42) }()
	<-r.entered
	finished := make(chan []int, 1)
	go func() { finished <- s.Finish(1) }()
	select {
	case <-finished:
		t.Fatal("finish crossed in-flight append")
	default:
	}
	close(r.release)
	require.True(t, <-appended)
	require.Equal(t, []int{42}, <-finished)
	assert.False(t, s.Append(1, 43))
	assert.Empty(t, s.Finish(1))
	assert.Zero(t, s.LiveTotal())
	s.Shutdown(func() {}, func() {})
	assert.False(t, s.Attach(2, NewRunner[int](func() {}, "", 0, admission.Parent, 0, false, nil)))
	assert.Zero(t, s.LiveTotal())
}

func TestSupervisorStopSignalsTreeBeforeProcessJoin(t *testing.T) {
	s := NewSupervisor[int](stopSessions{}, nil, nil, nil, nil)
	var phases []string
	stopper := &recordingStopper{phases: &phases}
	contexts := make([]context.Context, 0, 2)
	for _, id := range []int64{1, 2} {
		ctx, cancel := context.WithCancel(context.Background())
		contexts = append(contexts, ctx)
		require.True(t, s.Attach(id, NewRunner[int](cancel, "", 0, admission.Parent, 0, false, nil)))
	}
	effects := StopEffects{
		CancelProcesses: func(context.Context, int64) error {
			for _, ctx := range contexts {
				require.ErrorIs(t, ctx.Err(), context.Canceled)
			}
			phases = append(phases, "processes")
			s.Finish(1)
			s.Finish(2)
			return nil
		},
		RetireResources:  func(context.Context, int64) error { phases = append(phases, "resources"); return nil },
		SettleCalls:      func(context.Context, int64) error { phases = append(phases, "calls"); return nil },
		ExpireActivation: func(context.Context, int64) error { phases = append(phases, "activation"); return nil },
		CancelSleeps:     func(context.Context, int64) error { phases = append(phases, "sleeps"); return nil },
	}
	require.NoError(t, s.StopTree(t.Context(), 1, stopper, true, effects))
	assert.Equal(
		t,
		[]string{
			"begin",
			"processes",
			"resources",
			"inputs",
			"calls",
			"activation",
			"sleeps",
			"calls",
			"activation",
			"sleeps",
			"finish",
		},
		phases,
	)
	s.Shutdown(func() {}, func() {})
}

func TestSupervisorShutdownCancelsAndJoinsQueueRetry(t *testing.T) {
	var attempts atomic.Int64
	entered, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	s := NewSupervisor[int](nil, nil, nil, func(ctx context.Context, _ int64) (bool, error) {
		if attempts.Add(1) == 1 {
			return false, errors.New("unreadable child")
		}
		close(entered)
		<-ctx.Done()
		close(cancelled)
		<-release
		return false, ctx.Err()
	}, nil)
	s.QueueChild(t.Context(), 2, 1, "", 0)
	s.DrainChildren(t.Context())
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("retry did not start")
	}
	done := make(chan struct{})
	go func() { s.Shutdown(func() {}, func() {}); close(done) }()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("retry was not cancelled")
	}
	select {
	case <-done:
		t.Fatal("shutdown returned before retry joined")
	default:
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("retry did not join")
	}
}

func (r *blockedAppendRunner) AppendInput(input int) {
	close(r.entered)
	<-r.release
	r.Runner.AppendInput(input)
}

func (s registeringSessions) GetSession(ctx context.Context, id int64) (*sessionstore.SessionRecord, error) {
	close(s.entered)
	<-s.release
	return s.launcherSessions.GetSession(ctx, id)
}

func (stopSessions) ListAllSessions(context.Context) ([]*sessionstore.SessionRecord, error) {
	return []*sessionstore.SessionRecord{{ID: 1}, {ID: 2, RootID: 1}}, nil
}

func (s *recordingStopper) Begin(context.Context, int64, []int64) (*StopPlan, error) {
	*s.phases = append(*s.phases, "begin")
	return &StopPlan{rootID: 1, sessionIDs: []int64{1, 2}}, nil
}

func (s *recordingStopper) CancelInputs(context.Context, *StopPlan) error {
	*s.phases = append(*s.phases, "inputs")
	return nil
}

func (s *recordingStopper) Finish(context.Context, *StopPlan, bool) error {
	*s.phases = append(*s.phases, "finish")
	return nil
}
