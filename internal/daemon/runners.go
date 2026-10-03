package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/progressruntime"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
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

type refusal string

type waitingRunner struct {
	sessionID int64
	parentID  int64
	child     bool
}

type runnerSet struct {
	mu        sync.Mutex
	byID      map[int64]*runner
	closed    bool
	running   int
	children  int
	perParent map[int64]int
	waiting   []waitingRunner
	retrying  bool
	deferred  map[int64]bool
}

type runner struct {
	mu              sync.Mutex
	sessionID       int64
	projectID       int64
	parentID        int64
	workDir         string
	child           bool
	cancel          context.CancelFunc
	done            chan struct{}
	service         *session.Session
	working         bool
	hasRun          bool
	preserveStopped bool
}

func (s *svc) HasActiveLoop(id int64) bool { _, ok := s.runners.load(id); return ok }
func (r refusal) Error() string            { return string(r) }

func newRunnerSet() *runnerSet {
	return &runnerSet{byID: make(map[int64]*runner), perParent: make(map[int64]int), deferred: make(map[int64]bool)}
}

func (s *runnerSet) load(id int64) (*runner, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, ok := s.byID[id]

	return r, ok
}

func (s *runnerSet) register(r *runner) (*runner, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.byID[r.sessionID]; ok {
		return existing, false
	}

	if s.closed {
		return nil, false
	}

	s.byID[r.sessionID] = r

	return nil, true
}

func (s *runnerSet) remove(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if r, ok := s.byID[id]; ok {
		delete(s.byID, id)
		s.releaseLocked(r.child, r.parentID)
	}
}

func (s *runnerSet) closeAndSnapshot() []*runner {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closed = true

	runners := make([]*runner, 0, len(s.byID))
	for _, r := range s.byID {
		runners = append(runners, r)
	}

	return runners
}

func (s *runnerSet) tryAdmit(child bool, parentID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.canAdmit(child, parentID) {
		return false
	}

	s.running++
	if child {
		s.children++
		s.perParent[parentID]++
	}

	return true
}

func (s *runnerSet) release(child bool, parentID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.releaseLocked(child, parentID)
}

func (s *runnerSet) releaseLocked(child bool, parentID int64) {
	s.running--
	if child {
		s.children--

		s.perParent[parentID]--
		if s.perParent[parentID] == 0 {
			delete(s.perParent, parentID)
		}
	}
}

func (s *runnerSet) canAdmit(child bool, parentID int64) bool {
	return !s.closed && s.running < maxTotal &&
		(!child || s.children < maxChildren && s.perParent[parentID] < maxPerParent)
}

func (s *runnerSet) wait(w waitingRunner) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.closed && !slices.ContainsFunc(s.waiting, func(r waitingRunner) bool { return r.sessionID == w.sessionID }) {
		s.waiting = append(s.waiting, w)
	}
}

func (s *runnerSet) nextAdmissible() (waitingRunner, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, w := range s.waiting {
		if s.canAdmit(w.child, w.parentID) {
			s.waiting = slices.Delete(s.waiting, i, i+1)
			return w, true
		}
	}

	return waitingRunner{}, false
}

func (s *runnerSet) hasAdmissible() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.ContainsFunc(s.waiting, func(w waitingRunner) bool { return s.canAdmit(w.child, w.parentID) })
}

func (s *runnerSet) forget(ids []int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.waiting = slices.DeleteFunc(s.waiting, func(w waitingRunner) bool { return slices.Contains(ids, w.sessionID) })
}

func (s *runnerSet) scheduleRetry() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed || s.retrying {
		return false
	}

	s.retrying = true

	return true
}

func (s *runnerSet) retryDone() { s.mu.Lock(); defer s.mu.Unlock(); s.retrying = false }
func (s *runnerSet) deferAnnounced(id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.deferred[id]
}

func (s *runnerSet) recordDefer(id int64, announced bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if announced {
		s.deferred[id] = true
	} else {
		delete(s.deferred, id)
	}
}

func (s *svc) start(ctx context.Context, id int64) error {
	unlock, err := s.lockSessionTree(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()

	return s.startLocked(ctx, id)
}

func (s *svc) startLocked(ctx context.Context, id int64) error {
	if s.life.closed() {
		return errDaemonShuttingDown
	}

	if _, ok := s.runners.load(id); ok {
		return nil
	}

	rec, err := s.store.GetSession(ctx, id)
	if err != nil {
		return fmt.Errorf("load session %d before start: %w", id, err)
	}

	if rec.KilledAt != nil || rec.Status == sessionstore.SessionStatusKilled {
		return refusal(fmt.Sprintf("session %d is killed", id))
	}

	if rec.Status == sessionstore.SessionStatusStopping || rec.Status == sessionstore.SessionStatusTerminating {
		return refusal(fmt.Sprintf("session %d is %s", id, rec.Status))
	}

	preserve, err := s.commandOnlyStoppedRoot(ctx, rec)
	if err != nil {
		return err
	}

	if rec.Status == sessionstore.SessionStatusStopped && !preserve {
		return refusal(fmt.Sprintf("session %d is stopped", id))
	}

	link, err := s.links.GetLink(ctx, id)
	if err != nil {
		return fmt.Errorf("classify session %d: %w", id, err)
	}

	if link != nil && (link.Terminal() || link.State == subagent.StateStopped) {
		return nil
	}

	w := waitingRunner{sessionID: id, child: link != nil}
	if link != nil {
		w.parentID = link.ParentID
	}

	if !s.runners.tryAdmit(w.child, w.parentID) {
		if link != nil && link.Blocking {
			return errNoCapacity
		}

		s.runners.wait(w)

		if s.runners.hasAdmissible() {
			s.life.Go("daemon.admission", s.drain)
		}

		return nil
	}

	return s.startAdmitted(ctx, rec, w, preserve)
}

func (s *svc) startAdmitted(
	ctx context.Context,
	rec *sessionstore.SessionRecord,
	w waitingRunner,
	preserve bool,
) error {
	workDir, err := s.store.GetProjectWorkDir(ctx, rec.ProjectID)
	if err != nil {
		s.runners.release(w.child, w.parentID)
		return fmt.Errorf("resolve project %d: %w", rec.ProjectID, err)
	}

	if !s.life.enter() {
		s.runners.release(w.child, w.parentID)
		return errDaemonShuttingDown
	}

	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	rs := newRunner(cancel, workDir, rec, w, preserve)

	s.liveMu.Lock()
	s.progress.SetLive(rec.ID, progressruntime.Live{Active: true})

	_, registered := s.runners.register(rs)
	if !registered {
		s.updateLiveLocked(ctx, rec.ID)
	}
	s.liveMu.Unlock()

	if !registered {
		s.runners.release(w.child, w.parentID)
		cancel()
		s.life.leave()

		if s.life.closed() {
			return errDaemonShuttingDown
		}
		return nil
	}

	go s.runSession(loopCtx, rs)
	return nil
}

func (s *svc) drain(ctx context.Context) {
	for {
		w, ok := s.runners.nextAdmissible()
		if !ok {
			return
		}

		err := s.start(ctx, w.sessionID)
		if err == nil {
			continue
		}

		if errors.Is(err, errNoCapacity) {
			s.runners.wait(w)
			return
		}

		if _, ok := errors.AsType[refusal](err); ok {
			logger.Ctx(ctx).Named("daemon.admission").
				Info("skip_unstartable_waiting_runner", zap.Int64("session_id", w.sessionID), zap.Error(err))

			continue
		}

		s.runners.wait(w)
		logger.Ctx(ctx).Named("daemon.admission").
			Error("waiting_runner_start_failed", zap.Int64("session_id", w.sessionID), zap.Error(err))

		if s.runners.scheduleRetry() {
			s.life.Go("daemon.admission", func(retryCtx context.Context) {
				timer := time.NewTimer(100 * time.Millisecond)
				defer timer.Stop()

				select {
				case <-retryCtx.Done():
					s.runners.retryDone()
				case <-timer.C:
					s.runners.retryDone()
					s.drain(retryCtx)
				}
			})
		}

		return
	}
}

func (s *svc) removeRunner(ctx context.Context, rs *runner) {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()

	s.runners.remove(rs.sessionID)
	s.updateLiveLocked(ctx, rs.sessionID)
}

func (s *svc) releaseRunner(ctx context.Context, rs *runner) { s.removeRunner(ctx, rs); rs.Complete() }

func newRunner(
	cancel context.CancelFunc,
	workDir string,
	rec *sessionstore.SessionRecord,
	w waitingRunner,
	preserve bool,
) *runner {
	return &runner{
		sessionID: rec.ID, projectID: rec.ProjectID, parentID: w.parentID, child: w.child,
		workDir: workDir, cancel: cancel, done: make(chan struct{}), preserveStopped: preserve,
	}
}

func (r *runner) Cancel()                   { r.cancel() }
func (r *runner) Stop()                     { r.cancel(); <-r.done }
func (r *runner) Done() <-chan struct{}     { return r.done }
func (r *runner) Complete()                 { close(r.done) }
func (r *runner) Service() *session.Session { r.mu.Lock(); defer r.mu.Unlock(); return r.service }
func (r *runner) SetService(service *session.Session) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.service = service
}
func (r *runner) Working() bool           { r.mu.Lock(); defer r.mu.Unlock(); return r.working }
func (r *runner) SetWorking(working bool) { r.mu.Lock(); defer r.mu.Unlock(); r.working = working }
func (r *runner) SetPreserveStopped(preserve bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.preserveStopped = preserve
}

func (r *runner) PreserveStopped() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.preserveStopped }
func (r *runner) HasRun() bool          { r.mu.Lock(); defer r.mu.Unlock(); return r.hasRun }
func (r *runner) MarkRun()              { r.mu.Lock(); defer r.mu.Unlock(); r.hasRun = true }
