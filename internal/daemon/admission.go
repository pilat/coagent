package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
)

// Concurrency limits. Children are capped below the total so ≥(total-child)
// slots are always reservable by parents — a completing child can therefore
// always re-admit its (suspended, slot-free) parent, which kills the
// priority-inversion deadlock. Per-parent in-flight bounds fan-out; depth bounds
// nesting (root → child → grandchild).
const (
	maxTotalSlots        = 16
	maxChildSlots        = 12
	maxInFlightPerParent = 8
	maxSubagentDepth     = 3
)

// errNoCapacity is the only ensureRunner failure a caller may retry later; any
// other error means the session cannot start at all, so re-parking it would spin.
var errNoCapacity = errors.New("session capacity reached")

type slotKind int

const (
	slotParent slotKind = iota // root or any non-subagent session
	slotChild                  // a subagent session
)

// admissionCtl governs how many session loops run concurrently. It replaces the
// bare weighted semaphore with kind-aware, per-parent-bounded admission and
// atomic gauges for tests. Admission is non-blocking (tryAdmit), so a parent's
// spawn never waits on a slot.
type admissionCtl struct {
	mu           sync.Mutex
	running      int
	runningChild int
	perParent    map[int64]int

	totalGauge atomic.Int64
	childGauge atomic.Int64
}

func newAdmissionCtl() *admissionCtl {
	return &admissionCtl{perParent: make(map[int64]int)}
}

// tryAdmit reserves a slot if the relevant caps allow, returning success.
func (a *admissionCtl) tryAdmit(kind slotKind, parentID int64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.running >= maxTotalSlots {
		return false
	}

	if kind == slotChild {
		if a.runningChild >= maxChildSlots {
			return false
		}

		if a.perParent[parentID] >= maxInFlightPerParent {
			return false
		}
	}

	a.running++
	a.totalGauge.Store(int64(a.running))

	if kind == slotChild {
		a.runningChild++
		a.perParent[parentID]++
		a.childGauge.Store(int64(a.runningChild))
	}

	return true
}

// release frees a slot previously reserved by tryAdmit.
func (a *admissionCtl) release(kind slotKind, parentID int64) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.running > 0 {
		a.running--
	}

	a.totalGauge.Store(int64(a.running))

	if kind != slotChild {
		return
	}

	if a.runningChild > 0 {
		a.runningChild--
	}

	a.childGauge.Store(int64(a.runningChild))

	if a.perParent[parentID] > 0 {
		a.perParent[parentID]--
		if a.perParent[parentID] == 0 {
			delete(a.perParent, parentID)
		}
	}
}

// canAdmitChild reports whether a child of parentID could be admitted right now
// (used to peek before dequeuing a queued child).
func (a *admissionCtl) canAdmitChild(parentID int64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.running < maxTotalSlots &&
		a.runningChild < maxChildSlots &&
		a.perParent[parentID] < maxInFlightPerParent
}

func (a *admissionCtl) liveTotal() int64    { return a.totalGauge.Load() }
func (a *admissionCtl) liveChildren() int64 { return a.childGauge.Load() }

// enqueueChild parks a background child that could not be admitted, preserving
// its initial messages so the prompt survives until a slot frees.
func (s *svc) enqueueChild(
	ctx context.Context,
	sessionID, parentID int64,
	workDir string,
	projectID int64,
) {
	s.queueMu.Lock()
	s.queue = append(s.queue, queuedChild{
		sessionID: sessionID,
		parentID:  parentID,
		workDir:   workDir,
		projectID: projectID,
	})
	s.queueMu.Unlock()

	logger.Ctx(ctx).Named("daemon.admission").Info("subagent_queued", zap.Int64("child", sessionID))
}

// drainQueue starts one queued child whose parent now has capacity. Called after
// every slot release; ensureRunner re-checks admission (re-queueing on a race).
func (s *svc) drainQueue(ctx context.Context) {
	s.queueMu.Lock()

	idx := -1

	for i, q := range s.queue {
		if s.admit.canAdmitChild(q.parentID) {
			idx = i
			break
		}
	}

	if idx < 0 {
		s.queueMu.Unlock()
		return
	}

	next := s.queue[idx]
	s.queue = append(s.queue[:idx], s.queue[idx+1:]...)
	s.queueMu.Unlock()

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

		return
	}

	if terminated {
		logger.Ctx(ctx).Named("daemon.admission").Info("skip_killed_queued_child", zap.Int64("child", next.sessionID))
		s.drainQueue(ctx)

		return
	}

	err = s.ensureRunner(ctx, next.sessionID, next.workDir, next.projectID, nil)
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

// childTerminated reports whether a queued child was killed/terminalized before it
// got a runner (e.g. by cascadeKillChildren). Checked just before launch so a
// stale queue entry is never turned into a live runner. An unreadable ledger is
// neither answer, so the caller must defer the decision instead of guessing.
func (s *svc) childTerminated(ctx context.Context, childID int64) (bool, error) {
	link, err := s.links.GetLink(ctx, childID)
	if err != nil {
		return false, fmt.Errorf("queued child link %d: %w", childID, err)
	}

	if link != nil && (link.Terminal() || link.State == LinkStateStopped) {
		return true, nil
	}

	rec, err := s.sessionStore.GetSession(ctx, childID)
	if err != nil {
		return false, fmt.Errorf("queued child session %d: %w", childID, err)
	}

	return rec.KilledAt != nil, nil
}
