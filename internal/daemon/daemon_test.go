package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/progressruntime"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestManager_Shutdown(t *testing.T) {
	h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: (&mockFactory{}).client})
	mgr, s := h.mgr, h.store
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

// Recovery is background work, but it must exit before shutdown lets its stores close.
func TestShutdownCancelsBackgroundRecovery(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	links := &blockingRecoveryLinks{
		Store:       h.mgr.links,
		entered:     make(chan struct{}),
		cancelled:   make(chan struct{}),
		allowReturn: make(chan struct{}),
	}
	h.mgr.links = links
	require.NoError(t, h.mgr.Start(h.ctx))
	select {
	case <-links.entered:
	case <-time.After(time.Second):
		t.Fatal("background recovery did not reach the cancellation boundary")
	}
	shutdownDone := make(chan struct{})
	go func() {
		h.mgr.Shutdown(time.Second)
		close(shutdownDone)
	}()
	select {
	case <-links.cancelled:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel background recovery")
	}
	select {
	case <-shutdownDone:
		t.Fatal("shutdown returned before background recovery exited")
	default:
	}
	close(links.allowReturn)
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not wait for background recovery")
	}
}

// Runner cancellation must not wait for an unrelated background owner to join:
// the composition root may close persistence as soon as Shutdown returns.
func TestShutdownCancelsRunnersBeforeWaitingForProgress(t *testing.T) {
	h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: (&mockFactory{}).client})
	mgr := h.mgr
	runnerCtx, cancel := context.WithCancel(context.Background())
	activeRunner := newRunner(
		cancel, t.TempDir(), &sessionstore.SessionRecord{ID: 1}, waitingRunner{sessionID: 1}, false,
	)
	_, registered := mgr.runners.register(activeRunner)
	require.True(t, registered)
	go func() {
		<-runnerCtx.Done()
		activeRunner.Complete()
	}()
	progress := &blockingProgressStop{entered: make(chan struct{}), release: make(chan struct{})}
	mgr.progress = progress
	shutdownDone := make(chan struct{})
	go func() {
		mgr.Shutdown(time.Second)
		close(shutdownDone)
	}()
	<-progress.entered
	select {
	case <-runnerCtx.Done():
		close(progress.release)
	case <-time.After(250 * time.Millisecond):
		close(progress.release)
		<-shutdownDone
		t.Fatal("runner cancellation waited for progress shutdown")
	}
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not join the released progress owner and runner")
	}
}

func TestShutdownWaitsForStartupProcessRecovery(t *testing.T) {
	h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: (&mockFactory{}).client})
	mgr := h.mgr
	processes := &blockingStartupProcesses{
		Service: mgr.processes,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	mgr.processes = processes
	startupDone := make(chan error, 1)
	go func() { startupDone <- mgr.Start(t.Context()) }()
	requireBarrierSignal(t, processes.entered, "startup did not reach process recovery")
	shutdownDone := make(chan struct{})
	go func() { mgr.Shutdown(time.Second); close(shutdownDone) }()
	select {
	case <-shutdownDone:
		t.Fatal("shutdown returned before startup process recovery")
	case <-time.After(50 * time.Millisecond):
	}
	close(processes.release)
	require.ErrorIs(t, <-startupDone, errDaemonShuttingDown)
	requireBarrierSignal(t, shutdownDone, "shutdown did not join startup process recovery")
}

type blockingProgressStop struct {
	progressruntime.Service

	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingProgressStop) Stop(context.Context) error {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return nil
}

type blockingStartupProcesses struct {
	backgroundprocess.Service
	entered chan struct{}
	release chan struct{}
}

func (s *blockingStartupProcesses) InterruptNonterminal(ctx context.Context) (int, error) {
	close(s.entered)
	<-s.release
	return s.Service.InterruptNonterminal(ctx)
}

func runnerCounts(runners *runnerSet) (int, int) {
	runners.mu.Lock()
	defer runners.mu.Unlock()
	return runners.running, runners.children
}
