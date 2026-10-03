package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/subagent"
)

const childQueueRetryDelay = 100 * time.Millisecond

// queuedChild is a background child that could not be admitted immediately and
// waits (in arrival order) for a slot to free. Durability comes from its
// already-persisted subagent_links row (state 'spawned', inserted by Spawn before
// admission) — the restart sweep re-runs it on crash; this slice is only the
// in-memory ordering cache.
type queuedChild struct {
	sessionID int64
	parentID  int64
	workDir   string
	projectID int64
}

type queuedRunner struct {
	sessionID int64
	workDir   string
	projectID int64
}

type queue[T any] struct {
	mu     sync.Mutex
	values []T
}

func (q *queue[T]) Push(value T) {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.values = append(q.values, value)
}

func (q *queue[T]) PushUnique(value T, same func(T, T) bool) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	for _, existing := range q.values {
		if same(existing, value) {
			return false
		}
	}

	q.values = append(q.values, value)

	return true
}

func (q *queue[T]) PopFirst(predicate func(T) bool) (T, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	for i, value := range q.values {
		if !predicate(value) {
			continue
		}

		q.values = append(q.values[:i], q.values[i+1:]...)

		return value, true
	}

	var zero T

	return zero, false
}

func (q *queue[T]) Remove(predicate func(T) bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	kept := q.values[:0]
	for _, value := range q.values {
		if !predicate(value) {
			kept = append(kept, value)
		}
	}

	q.values = kept
}

func (q *queue[T]) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()

	return len(q.values)
}

func newQueue[T any]() *queue[T] { return &queue[T]{} }

// enqueueChild parks a background child that could not be admitted, preserving
// its initial messages so the prompt survives until a slot frees.
func (s *svc) enqueueChild(
	ctx context.Context,
	sessionID, parentID int64,
	workDir string,
	projectID int64,
) {
	s.childQueue.Push(queuedChild{
		sessionID: sessionID,
		parentID:  parentID,
		workDir:   workDir,
		projectID: projectID,
	})

	logger.Ctx(ctx).Named("daemon.admission").Info("subagent_queued", zap.Int64("child", sessionID))
}

func (s *svc) enqueueCapacityBlockedChild(
	ctx context.Context,
	sessionID, parentID int64,
	workDir string,
	projectID int64,
) {
	s.enqueueChild(ctx, sessionID, parentID, workDir, projectID)

	if s.admit.canAdmit(true, parentID) {
		go s.drainQueue(context.WithoutCancel(ctx))
	}
}

// drainQueue starts one queued child whose parent now has capacity. Called after
// every slot release; ensureRunner re-checks admission (re-queueing on a race).
func (s *svc) drainQueue(ctx context.Context) {
	next, ok := s.childQueue.PopFirst(func(queued queuedChild) bool {
		return s.admit.canAdmit(true, queued.parentID)
	})
	if !ok {
		return
	}

	// A child cascade-killed while parked (its link/session marked terminal by
	// killSubagent, but no runner existed to stop) must never be launched. It is
	// already removed from the queue above; skip it and try the next entry.
	terminated, err := s.childTerminated(ctx, next.sessionID)
	if err != nil {
		// Recursing on an unknown state would quietly drain the whole queue, so
		// park the entry instead and let the next slot release retry it.
		logger.Ctx(ctx).Named("daemon.admission").
			Error("queued_child_state_unknown", zap.Int64("child", next.sessionID), zap.Error(err))
		s.enqueueChild(ctx, next.sessionID, next.parentID, next.workDir, next.projectID)
		s.scheduleChildQueueRetry(ctx)

		return
	}

	if terminated {
		logger.Ctx(ctx).Named("daemon.admission").Info("skip_killed_queued_child", zap.Int64("child", next.sessionID))
		s.drainQueue(ctx)

		return
	}

	err = s.ensureRunner(ctx, next.sessionID, next.workDir, next.projectID)
	if errors.Is(err, errNoCapacity) {
		// Admission lost a race — park it again for the next release.
		s.enqueueChild(ctx, next.sessionID, next.parentID, next.workDir, next.projectID)

		return
	}

	if err != nil {
		// Anything else is not a race, so re-parking would only spin on it.
		logger.Ctx(ctx).Named("daemon.admission").
			Error("queued_child_start_failed", zap.Int64("child", next.sessionID), zap.Error(err))
	}
}

func (s *svc) scheduleChildQueueRetry(ctx context.Context) {
	s.queueRetryMu.Lock()
	if s.shuttingDown.Load() || s.queueRetryPending {
		s.queueRetryMu.Unlock()

		return
	}

	s.queueRetryPending = true
	s.workerWG.Add(1)
	s.queueRetryMu.Unlock()

	go func() {
		defer s.workerWG.Done()

		retryCtx, cancel := s.newDaemonWorkerContext(ctx)
		defer cancel()

		timer := time.NewTimer(childQueueRetryDelay)
		defer timer.Stop()

		select {
		case <-timer.C:
		case <-retryCtx.Done():
		}

		s.queueRetryMu.Lock()
		s.queueRetryPending = false
		s.queueRetryMu.Unlock()

		if retryCtx.Err() == nil && !s.shuttingDown.Load() {
			s.drainQueue(retryCtx)
		}
	}()
}

// childTerminated reports whether a queued child was killed/terminalized before it
// got a runner (e.g. by cascadeKillChildren). Checked just before launch so a
// stale queue entry is never turned into a live runner. An unreadable ledger is
// neither answer, so the caller must defer the decision instead of guessing.
func (s *svc) childTerminated(ctx context.Context, childID int64) (bool, error) {
	link, err := s.links.GetLink(ctx, childID)
	if err != nil {
		return false, fmt.Errorf("queued child link %d: %w", childID, err)
	}

	if link != nil && (link.Terminal() || link.State == subagent.StateStopped) {
		return true, nil
	}

	rec, err := s.store.GetSession(ctx, childID)
	if err != nil {
		return false, fmt.Errorf("queued child session %d: %w", childID, err)
	}

	return rec.KilledAt != nil, nil
}
