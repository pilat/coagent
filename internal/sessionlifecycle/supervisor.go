package sessionlifecycle

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/admission"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

const queueRetryDelay = 100 * time.Millisecond

var _ Supervisor[int] = (*supervisor[int])(nil)

type queuedSession struct {
	sessionID, parentID, projectID int64
	workDir                        string
}

// Supervisor owns admission and the lifetime of reconstructible runners.
//
//nolint:interfacebloat // One owner coordinates admission, runner lifetime and tree fencing.
type Supervisor[T any] interface {
	Lookup(int64) (Runner[T], bool)
	Count() int
	LiveTotal() int64
	LiveChildren() int64
	QueuedChildren() int
	QueuedRoots() int
	Append(int64, ...T) bool
	Attach(int64, Runner[T]) bool
	Finish(int64) []T
	Ensure(context.Context, int64, string, int64, []T) error
	EnsureLocked(context.Context, int64, string, int64, []T) error
	LockTree(context.Context, int64) (func(), error)
	QueueChild(context.Context, int64, int64, string, int64)
	QueueRoot(int64, string, int64)
	DrainChildren(context.Context)
	DrainRoots(context.Context)
	DrainReady(context.Context)
	RemoveQueued([]int64)
	Shutdown(func(), func())
	StopTree(context.Context, int64, Stopper, bool, StopEffects) error
}

type supervisor[T any] struct {
	runners      Registry[Runner[T]]
	admit        admission.Governor
	launcher     Launcher[T]
	sessions     sessionstore.OrchestrationStore
	children     Queue[queuedSession]
	roots        Queue[queuedSession]
	locks        sync.Map
	terminated   func(context.Context, int64) (bool, error)
	retryMu      sync.Mutex
	retryPending bool
	retryCtx     context.Context //nolint:containedctx // Supervisor lifetime cancels and joins all queue workers.
	retryCancel  context.CancelFunc
	retryWG      sync.WaitGroup
}

func NewSupervisor[T any](
	sessions sessionstore.OrchestrationStore,
	links subagent.Store,
	startable func(context.Context, *sessionstore.SessionRecord, []T) (bool, error),
	terminated func(context.Context, int64) (bool, error),
	run func(context.Context, int64, Runner[T]),
) Supervisor[T] {
	s := &supervisor[T]{
		runners: NewRegistry[Runner[T]](), admit: admission.New(), sessions: sessions,
		children: NewQueue[queuedSession](), roots: NewQueue[queuedSession](),
		terminated: terminated,
	}
	//nolint:gosec // Shutdown calls retryCancel before joining queue workers.
	s.retryCtx, s.retryCancel = context.WithCancel(context.Background())
	s.launcher = NewLauncher(sessions, links, s.admit, s.runners, startable, s.queueBlockedChild, run)

	return s
}

func (s *supervisor[T]) Lookup(id int64) (Runner[T], bool) { return s.runners.Load(id) }
func (s *supervisor[T]) Count() int                        { return s.runners.Len() }
func (s *supervisor[T]) LiveTotal() int64                  { return s.admit.LiveTotal() }
func (s *supervisor[T]) LiveChildren() int64               { return s.admit.LiveChildren() }
func (s *supervisor[T]) QueuedChildren() int               { return s.children.Len() }
func (s *supervisor[T]) QueuedRoots() int                  { return s.roots.Len() }

func (s *supervisor[T]) Append(id int64, inputs ...T) bool {
	return s.runners.Use(id, func(r Runner[T]) {
		for _, input := range inputs {
			r.AppendInput(input)
		}
	})
}

// Attach admits an already assembled runner; the caller owns starting its work.
func (s *supervisor[T]) Attach(id int64, r Runner[T]) bool {
	info := r.Info()
	if !s.admit.TryAdmit(info.Kind, info.ParentID) {
		return false
	}

	_, registered := s.runners.Register(id, r)
	if !registered {
		s.admit.Release(info.Kind, info.ParentID)
	}

	return registered
}

// Finish removes the append boundary before draining and exposing cancellation completion.
func (s *supervisor[T]) Finish(id int64) []T {
	r, ok := s.runners.Delete(id)
	if !ok {
		return nil
	}

	info := r.Info()
	s.admit.Release(info.Kind, info.ParentID)

	inputs := r.DrainInputs()
	r.Complete()

	return inputs
}

func (s *supervisor[T]) Ensure(ctx context.Context, id int64, dir string, projectID int64, inputs []T) error {
	if s.runners.Closed() {
		return ErrShuttingDown
	}

	unlock, err := s.LockTree(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()

	return s.EnsureLocked(ctx, id, dir, projectID, inputs)
}

func (s *supervisor[T]) EnsureLocked(ctx context.Context, id int64, dir string, projectID int64, inputs []T) error {
	if err := s.launcher.Ensure(ctx, id, dir, projectID, inputs); err != nil {
		return fmt.Errorf("ensure session runner: %w", err)
	}

	return nil
}

func (s *supervisor[T]) LockTree(ctx context.Context, id int64) (func(), error) {
	record, err := s.sessions.GetSession(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("load session tree lock: %w", err)
	}

	rootID := record.RootID
	if rootID == 0 {
		rootID = record.ID
	}

	candidate := make(chan struct{}, 1)
	candidate <- struct{}{}

	value, _ := s.locks.LoadOrStore(rootID, candidate)

	lock, ok := value.(chan struct{})
	if !ok {
		return nil, fmt.Errorf("invalid session tree lock for root %d", rootID)
	}

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("acquire session tree lock: %w", ctx.Err())
	case <-lock:
		return func() { lock <- struct{}{} }, nil
	}
}

func (s *supervisor[T]) QueueChild(ctx context.Context, id, parentID int64, dir string, projectID int64) {
	s.children.Push(queuedSession{sessionID: id, parentID: parentID, workDir: dir, projectID: projectID})
	logger.Ctx(ctx).Named("sessionlifecycle.admission").Info("subagent_queued", zap.Int64("child", id))
}

func (s *supervisor[T]) QueueRoot(id int64, dir string, projectID int64) {
	s.roots.PushUnique(queuedSession{sessionID: id, workDir: dir, projectID: projectID},
		func(a, b queuedSession) bool { return a.sessionID == b.sessionID })
}

func (s *supervisor[T]) DrainChildren(ctx context.Context) {
	for {
		next, ok := s.children.PopFirst(func(q queuedSession) bool { return s.admit.CanAdmitChild(q.parentID) })
		if !ok {
			return
		}

		terminated, err := s.terminated(ctx, next.sessionID)
		if err != nil {
			logger.Ctx(ctx).
				Named("sessionlifecycle.admission").
				Error("queued_child_state_unknown", zap.Int64("child", next.sessionID), zap.Error(err))
			s.QueueChild(ctx, next.sessionID, next.parentID, next.workDir, next.projectID)
			s.scheduleRetry(ctx)

			return
		}

		if terminated {
			continue
		}

		err = s.Ensure(ctx, next.sessionID, next.workDir, next.projectID, nil)
		if errors.Is(err, admission.ErrNoCapacity) {
			s.QueueChild(ctx, next.sessionID, next.parentID, next.workDir, next.projectID)
			return
		}

		if err != nil {
			logger.Ctx(ctx).
				Named("sessionlifecycle.admission").
				Error("queued_child_start_failed", zap.Int64("child", next.sessionID), zap.Error(err))
		}

		return
	}
}

func (s *supervisor[T]) DrainRoots(ctx context.Context) {
	next, ok := s.roots.PopFirst(func(queuedSession) bool { return true })
	if !ok {
		return
	}

	err := s.Ensure(ctx, next.sessionID, next.workDir, next.projectID, nil)
	if errors.Is(err, admission.ErrNoCapacity) {
		s.QueueRoot(next.sessionID, next.workDir, next.projectID)
		return
	}

	if err != nil {
		logger.Ctx(ctx).
			Named("sessionlifecycle.admission").
			Error("pending_runner_start_failed", zap.Int64("session_id", next.sessionID), zap.Error(err))
	}
}

func (s *supervisor[T]) DrainReady(ctx context.Context) {
	s.DrainRoots(ctx)
	s.DrainChildren(ctx)
}

func (s *supervisor[T]) RemoveQueued(ids []int64) {
	removed := make(map[int64]bool, len(ids))
	for _, id := range ids {
		removed[id] = true
	}

	predicate := func(q queuedSession) bool { return removed[q.sessionID] }
	s.children.Remove(predicate)
	s.roots.Remove(predicate)
}

// Shutdown seals registration and signals every runner before joining other owners.
func (s *supervisor[T]) Shutdown(beforeJoin, afterJoin func()) {
	s.retryMu.Lock()
	s.retryCancel()
	runners := s.runners.CloseAndSnapshot()
	s.retryMu.Unlock()

	for _, r := range runners {
		r.Cancel()
	}

	beforeJoin()

	for _, r := range runners {
		<-r.Done()
	}

	s.retryWG.Wait()
	afterJoin()
}

func (s *supervisor[T]) queueBlockedChild(ctx context.Context, id, parentID int64, dir string, projectID int64) {
	s.QueueChild(ctx, id, parentID, dir, projectID)

	if s.admit.CanAdmitChild(parentID) {
		s.retryMu.Lock()
		if s.retryCtx.Err() == nil {
			s.retryWG.Go(func() {
				workerCtx, cancel := s.workerContext(ctx)
				defer cancel()

				s.DrainChildren(workerCtx)
			})
		}
		s.retryMu.Unlock()
	}
}

func (s *supervisor[T]) scheduleRetry(ctx context.Context) {
	s.retryMu.Lock()
	if s.retryCtx.Err() != nil || s.retryPending {
		s.retryMu.Unlock()
		return
	}

	s.retryPending = true
	s.retryWG.Add(1)

	s.retryMu.Unlock()
	go func() {
		defer s.retryWG.Done()

		timer := time.NewTimer(queueRetryDelay)
		defer timer.Stop()

		select {
		case <-timer.C:
		case <-s.retryCtx.Done():
		}

		s.retryMu.Lock()
		s.retryPending = false
		s.retryMu.Unlock()

		if s.retryCtx.Err() == nil {
			workerCtx, cancel := s.workerContext(ctx)
			defer cancel()

			s.DrainChildren(workerCtx)
		}
	}()
}

func (s *supervisor[T]) workerContext(ctx context.Context) (context.Context, context.CancelFunc) {
	workerCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(s.retryCtx, cancel)

	return workerCtx, func() { stop(); cancel() }
}
