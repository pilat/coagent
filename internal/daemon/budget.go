package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
)

const (
	budgetParkRequested = "requested"
	budgetParkDraining  = "draining"
)

var errUnpricedModel = errors.New("model has no catalog pricing")

type budgetClock struct {
	mu     sync.Mutex
	timers map[int64]*budgetTimer
	due    map[int64]struct{}
	signal chan struct{}
	closed bool
}

type budgetTimer struct {
	generation int64
	park       bool
	retry      bool
	t          *time.Timer
}

func sessionRootID(record *sessionstore.SessionRecord) int64 {
	if record.RootID != 0 {
		return record.RootID
	}

	return record.ID
}

func (s *svc) releaseArmedBudget(ctx context.Context, rootID int64, reason string) error {
	record, err := s.budgets.Get(ctx, rootID)
	if errors.Is(err, budget.ErrNotFound) ||
		(err == nil && record.State != budget.Armed) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("load budget for terminal release: %w", err)
	}

	if _, err := s.budgets.Release(ctx, rootID, record.Generation, reason); err != nil {
		return fmt.Errorf("release terminal budget: %w", err)
	}

	return nil
}

func (s *svc) settleRootBudget(
	ctx context.Context,
	rootID int64,
	suspended bool,
	runErr error,
) error {
	if suspended && runErr == nil {
		return nil
	}

	if runErr != nil {
		return s.releaseArmedBudget(ctx, rootID, "error")
	}

	retain, projectionErr := s.retainBudgetForBackground(ctx, rootID)
	if projectionErr != nil {
		logger.Ctx(ctx).Named("daemon.runner").Error(
			"background_obligation_projection_failed",
			zap.Int64("session_id", rootID), zap.Error(projectionErr),
		)
		s.publish(rootID, sessionevent.Notification{
			Type:    sessionevent.NotifyMessage,
			Message: "⚠️ Could not verify background work before releasing the active budget; the budget remains armed.",
		})

		return nil
	}

	if retain {
		return nil
	}

	return s.releaseArmedBudget(ctx, rootID, "completed")
}

func (s *svc) retainBudgetForBackground(ctx context.Context, rootID int64) (bool, error) {
	record, err := s.budgets.Get(ctx, rootID)
	if errors.Is(err, budget.ErrNotFound) ||
		(err == nil && record.State != budget.Armed) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("load budget for background projection: %w", err)
	}

	retained, err := s.store.HasBackgroundObligationByRoot(ctx, rootID)
	if err != nil {
		return true, fmt.Errorf("project background obligation: %w", err)
	}

	return retained, nil
}

func (s *svc) settleRunBudget(
	ctx context.Context,
	rec *sessionstore.SessionRecord,
	result session.RunResult,
	runErr error,
) error {
	if result.BudgetFired {
		record, budgetErr := s.budgets.Get(ctx, sessionRootID(rec))
		if budgetErr != nil {
			runErr = errors.Join(runErr, budgetErr)
		} else if record.ParkPhase == budgetParkRequested {
			s.startPark(record)
		}
	}

	if rec.ParentID == 0 && !result.BudgetFired {
		runErr = errors.Join(runErr, s.settleRootBudget(ctx, rec.ID, result.Suspended, runErr))
	}

	return runErr
}

func newBudgetClock() *budgetClock {
	return &budgetClock{
		timers: make(map[int64]*budgetTimer),
		due:    make(map[int64]struct{}),
		signal: make(chan struct{}, 1),
	}
}

func (c *budgetClock) set(record *budget.Record) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return
	}

	previous := c.timers[record.RootSessionID]
	armed := record.State == budget.Armed && record.DurationSeconds != nil

	parking := record.State == budget.Fired &&
		(record.ParkPhase == budgetParkRequested || record.ParkPhase == budgetParkDraining)
	if previous != nil && !previous.retry && (armed || parking) && previous.generation == record.Generation &&
		previous.park == parking {
		return
	}

	if previous != nil {
		previous.t.Stop()
		delete(c.timers, record.RootSessionID)
	}

	if !armed && !parking {
		return
	}

	deadline := time.Now().Add(time.Second)
	if armed {
		deadline = record.ArmedAt.Add(time.Duration(*record.DurationSeconds) * time.Second)
	}

	c.install(record.RootSessionID, &budgetTimer{generation: record.Generation, park: parking}, time.Until(deadline))
}

func (c *budgetClock) retry(rootID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return
	}

	previous := c.timers[rootID]
	if previous != nil && previous.retry {
		return
	}

	if previous != nil {
		previous.t.Stop()
	}

	c.install(rootID, &budgetTimer{retry: true}, time.Second)
}

func (c *budgetClock) install(rootID int64, timer *budgetTimer, delay time.Duration) {
	c.timers[rootID] = timer
	timer.t = time.AfterFunc(delay, func() {
		c.mu.Lock()
		defer c.mu.Unlock()

		if c.closed || c.timers[rootID] != timer {
			return
		}

		delete(c.timers, rootID)

		c.due[rootID] = struct{}{}
		select {
		case c.signal <- struct{}{}:
		default:
		}
	})
}

func (c *budgetClock) dueSignal() <-chan struct{} { return c.signal }

func (c *budgetClock) takeDue() []int64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	ids := make([]int64, 0, len(c.due))
	for id := range c.due {
		ids = append(ids, id)
	}

	clear(c.due)
	slices.Sort(ids)

	return ids
}

func (c *budgetClock) close() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.closed = true
	for _, timer := range c.timers {
		timer.t.Stop()
	}

	clear(c.timers)
	clear(c.due)
}

func (s *svc) refreshBudget(ctx context.Context, rootID int64) {
	record, err := s.store.Get(ctx, rootID)
	if errors.Is(err, budget.ErrNotFound) {
		return
	}

	if err != nil {
		logger.Ctx(ctx).Named("daemon.budget").Warn("load_timer_budget", zap.Error(err))
		s.clock.retry(rootID)

		return
	}

	s.clock.set(record)
}

func (s *svc) budgetDue(ctx context.Context, rootID int64) {
	record, err := s.store.Get(ctx, rootID)
	if errors.Is(err, budget.ErrNotFound) {
		return
	}

	if err != nil {
		logger.Ctx(ctx).Named("daemon.budget").Warn("load_timer_budget", zap.Error(err))
		s.clock.retry(rootID)

		return
	}

	if record.State == budget.Fired &&
		(record.ParkPhase == budgetParkRequested || record.ParkPhase == budgetParkDraining) {
		s.startPark(record)
		return
	}

	if record.State == budget.Armed && record.DurationSeconds != nil &&
		!time.Now().Before(record.ArmedAt.Add(time.Duration(*record.DurationSeconds)*time.Second)) {
		observed, fired, err := s.store.ObserveBudget(ctx, rootID, time.Now().UTC(), "")
		if err != nil {
			logger.Ctx(ctx).Named("daemon.budget").Warn("deadline_observation_failed", zap.Error(err))
			s.clock.retry(rootID)

			return
		}

		if fired && observed.ParkPhase == budgetParkRequested {
			s.startPark(observed)
		} else {
			s.clock.set(observed)
		}

		return
	}

	s.clock.set(record)
}

// checkBudgetModel refuses an unpriced model for an armed cost-limited tree.
func (s *svc) checkBudgetModel(ctx context.Context, rootID int64, model string) error {
	record, err := s.budgets.Get(ctx, rootID)
	if errors.Is(err, budget.ErrNotFound) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("load tree budget: %w", err)
	}

	if record.State == budget.Armed && record.CostLimitUSD != nil && !s.models.priced(model) {
		return errUnpricedModel
	}

	return nil
}

func (s *svc) reconcileArmedBudgets(ctx context.Context) error {
	armed, err := s.budgets.ListArmed(ctx)
	if err != nil {
		return fmt.Errorf("list armed budgets: %w", err)
	}

	for _, record := range armed {
		observed, fired, err := s.store.ObserveBudget(ctx, record.RootSessionID, time.Now().UTC(), "")
		if err != nil {
			return fmt.Errorf("reconcile budget %d: %w", record.RootSessionID, err)
		}

		if !fired {
			s.clock.set(observed)
		}
	}

	return nil
}

func (s *svc) parkBudgetTree(ctx context.Context, record *budget.Record) {
	if record == nil || record.State != budget.Fired || record.ParkOwner == "" {
		return
	}
	defer s.refreshBudget(context.WithoutCancel(ctx), record.RootSessionID)

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	owner := record.ParkOwner
	if record.ParkPhase == budgetParkRequested {
		_, err := s.budgets.BeginDrain(ctx, record.RootSessionID, record.Generation, owner)
		if errors.Is(err, budget.ErrConflict) {
			return
		}

		if err != nil {
			logger.Ctx(ctx).Named("daemon.budget").Warn("begin_drain_failed", zap.Error(err))
			return
		}
	}

	unlock, err := s.awaitParkTree(ctx, record.RootSessionID)
	if err != nil {
		if ctx.Err() == nil {
			logger.Ctx(ctx).Named("daemon.budget").Warn("lock_park_tree_failed", zap.Error(err))
		}

		return
	}

	defer unlock()

	current, err := s.budgets.Get(ctx, record.RootSessionID)
	if err != nil {
		logger.Ctx(ctx).Named("daemon.budget").Warn("load_park_generation", zap.Error(err))
		return
	}

	if current.Generation != record.Generation || current.State != budget.Fired || current.ParkOwner != owner ||
		(current.ParkPhase != budgetParkRequested && current.ParkPhase != budgetParkDraining) {
		return
	}

	if err := s.stopTreeCleanup(ctx, record.RootSessionID, stopTreeOptions{
		preserveBackgroundProcesses: true,
	}); err != nil {
		logger.Ctx(ctx).Named("daemon.budget").Warn("park_cleanup_failed", zap.Error(err))
		return
	}

	if _, err := s.budgets.MarkParked(ctx, record.RootSessionID, record.Generation, owner); err != nil {
		logger.Ctx(ctx).Named("daemon.budget").Warn("mark_parked_failed", zap.Error(err))
		return
	}

	s.progress.ReconcileLatestReadiness(ctx, record.RootSessionID)
}

func (s *svc) startPark(record *budget.Record) {
	s.life.Go("daemon.budget", func(ctx context.Context) { s.parkBudgetTree(ctx, record) })
}

func (s *svc) awaitParkTree(ctx context.Context, rootID int64) (func(), error) {
	for {
		t, err := s.loadTree(ctx, rootID)
		if err != nil {
			return nil, err
		}

		for _, r := range s.liveRunners(t) {
			select {
			case <-r.Done():
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		unlock, err := s.lockSessionTree(ctx, rootID)
		if err != nil {
			return nil, err
		}

		t, err = s.loadTree(ctx, rootID)
		if err != nil {
			unlock()
			return nil, err
		}

		if len(s.liveRunners(t)) == 0 {
			return unlock, nil
		}

		unlock()
	}
}
