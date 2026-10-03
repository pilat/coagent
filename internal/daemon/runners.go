package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/progressruntime"
	"github.com/pilat/coagent/internal/sessionbuild"
)

const (
	maxTotal     = 16
	maxChildren  = 12
	maxPerParent = 8
	maxDepth     = 3
)

var (
	errDaemonShuttingDown = errors.New("daemon is shutting down")
	errNoCapacity         = errors.New("session capacity reached")
)

type slots struct {
	mu        sync.Mutex
	running   int
	children  int
	perParent map[int64]int
}

type registry[T any] struct {
	mu     sync.Mutex
	values map[int64]T
	closed bool
}

func (s *svc) HasActiveLoop(sessionID int64) bool {
	_, ok := s.runners.Load(sessionID)

	return ok
}

func (s *slots) tryAdmit(child bool, parentID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running >= maxTotal || child && (s.children >= maxChildren || s.perParent[parentID] >= maxPerParent) {
		return false
	}

	s.running++
	if child {
		s.children++
		s.perParent[parentID]++
	}

	return true
}

func (s *slots) release(child bool, parentID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running > 0 {
		s.running--
	}

	if !child {
		return
	}

	if s.children > 0 {
		s.children--
	}

	if s.perParent[parentID] > 0 {
		s.perParent[parentID]--
		if s.perParent[parentID] == 0 {
			delete(s.perParent, parentID)
		}
	}
}

func (s *slots) canAdmit(child bool, parentID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.running < maxTotal && (!child || s.children < maxChildren && s.perParent[parentID] < maxPerParent)
}

func (s *svc) Shutdown(timeout time.Duration) {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	s.shuttingDown.Store(true)
	s.queueRetryMu.Lock()
	if s.workerCancel != nil {
		s.workerCancel()
	}
	s.queueRetryMu.Unlock()

	s.budgetTimerMu.Lock()
	if s.budgetCancel != nil {
		s.budgetCancel()
	}
	s.budgetTimerMu.Unlock()

	recoveryDone := s.stopRecovery()
	processRecoveryDone := s.currentProcessRecovery()

	runners := s.runners.CloseAndSnapshot()

	done := make(chan struct{})

	go func() {
		for _, rs := range runners {
			rs.Cancel()
		}

		// Controlled shutdown cancels and joins every owned process group
		// through the shared lifecycle service; their completions stay owed
		// as interrupted for the next startup.

		if _, err := s.processSvc.CancelAll(shutdownCtx, backgroundprocess.IntentDaemonShutdown); err != nil {
			logger.Ctx(shutdownCtx).Named("daemon.process").Warn("shutdown_cancel_failed", zap.Error(err))
		}

		_ = s.progress.Stop(shutdownCtx)

		for _, rs := range runners {
			<-rs.Done()
		}

		if recoveryDone != nil {
			<-recoveryDone
		}

		if processRecoveryDone != nil {
			<-processRecoveryDone
		}

		s.budgetWG.Wait()
		s.workerWG.Wait()

		if err := sessionbuild.CloseToolResources(s.buildInput.Resources); err != nil {
			logger.Named("manager.shutdown").Warn("close_tool_resources", zap.Error(err))
		}

		close(done)
	}()

	select {
	case <-done:
	case <-shutdownCtx.Done():
		logger.Named("manager.shutdown").Warn("shutdown_timeout", zap.Int("remaining_sessions", len(runners)))
	}
}

func (r *registry[T]) Load(sessionID int64) (T, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	value, ok := r.values[sessionID]

	return value, ok
}

func (r *registry[T]) Use(sessionID int64, fn func(T)) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	value, ok := r.values[sessionID]
	if !ok {
		return false
	}

	fn(value)

	return true
}

func (r *registry[T]) Register(sessionID int64, value T) (T, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if existing, ok := r.values[sessionID]; ok {
		return existing, false
	}

	if r.closed {
		var zero T

		return zero, false
	}

	r.values[sessionID] = value

	var zero T

	return zero, true
}

func (r *registry[T]) Delete(sessionID int64) (T, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	existing, ok := r.values[sessionID]
	if !ok {
		var zero T

		return zero, false
	}

	delete(r.values, sessionID)

	return existing, true
}

func (r *registry[T]) CloseAndSnapshot() []T {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.closed = true

	values := make([]T, 0, len(r.values))
	for _, value := range r.values {
		values = append(values, value)
	}

	return values
}

func (r *registry[T]) Closed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.closed
}

func (r *registry[T]) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.values)
}

func (s *svc) stopRecovery() <-chan struct{} {
	return s.recovery.Close()
}

func newRegistry[T any]() *registry[T] { return &registry[T]{values: make(map[int64]T)} }

func (s *svc) launchRunner(
	ctx context.Context,
	sessionID int64,
	workDir string,
	projectID int64,
) error {
	if s.runners.Closed() {
		return errDaemonShuttingDown
	}

	record, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("load session %d before start: %w", sessionID, err)
	}

	preserveStopped, err := s.ensureRunnerStartable(ctx, record)
	if err != nil {
		return err
	}

	if s.appendIfRunning(sessionID) {
		return nil
	}

	child, parentID, blocking, err := s.slotInfo(ctx, sessionID)
	if err != nil {
		return err
	}

	if !s.admit.tryAdmit(child, parentID) {
		if child && !blocking {
			s.enqueueCapacityBlockedChild(ctx, sessionID, parentID, workDir, projectID)

			return nil
		}

		return errNoCapacity
	}

	loopCtx, cancel := context.WithCancel(context.Background())
	runner := newRunner(cancel, workDir, projectID, child, parentID, preserveStopped)

	existing, registered := s.registerRunner(ctx, sessionID, runner)
	if !registered {
		s.admit.release(child, parentID)
		cancel()

		if existing == nil {
			return errDaemonShuttingDown
		}

		return nil
	}

	go s.runSession(loopCtx, sessionID, runner) //nolint:contextcheck // Runner lifetime must outlive the request.

	return nil
}

func (s *svc) appendIfRunning(sessionID int64) bool {
	_, running := s.runners.Load(sessionID)
	return running
}

func (s *svc) slotInfo(
	ctx context.Context,
	sessionID int64,
) (bool, int64, bool, error) {
	link, err := s.links.GetLink(ctx, sessionID)
	if err != nil {
		return false, 0, false, fmt.Errorf("classify session %d: %w", sessionID, err)
	}

	if link == nil {
		return false, 0, false, nil
	}

	return true, link.ParentID, link.Blocking, nil
}

func (s *svc) registerRunner(ctx context.Context, sessionID int64, runner *runner) (*runner, bool) {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()

	s.progress.SetLive(sessionID, progressruntime.Live{Active: true})

	existing, registered := s.runners.Register(sessionID, runner)
	if !registered {
		s.updateLiveLocked(ctx, sessionID)
	}

	return existing, registered
}

func (s *svc) removeRunner(ctx context.Context, sessionID int64) {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()

	s.runners.Delete(sessionID)
	s.updateLiveLocked(ctx, sessionID)
}
