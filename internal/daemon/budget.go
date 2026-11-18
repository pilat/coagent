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
	"github.com/pilat/coagent/internal/orchestrator"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
)

var errUnpricedModel = errors.New("model has no catalog pricing")

type parkSet struct {
	mu    sync.Mutex
	roots map[int64]bool // Retain triggers arriving while the current park exits.
}

type budgetClock struct {
	mu     sync.Mutex
	timers map[int64]*budgetTimer
	due    map[int64]struct{}
	signal chan struct{}
	closed bool
}

type budgetTimer struct {
	generation int64
	retry      bool
	t          *time.Timer
}

func sessionRootID(record *sessionstore.SessionRecord) int64 {
	if record.RootID != 0 {
		return record.RootID
	}

	return record.ID
}

func (s *Service) releaseArmedBudget(ctx context.Context, rootID int64, reason string) error {
	record, err := s.budgets.Get(ctx, rootID)
	if errors.Is(err, budget.ErrNotFound) ||
		(err == nil && record.State != budget.Armed) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("load budget for terminal release: %w", err)
	}

	_, err = s.budgets.Release(ctx, rootID, record.Generation, reason)
	if errors.Is(err, budget.ErrConflict) {
		logger.Ctx(ctx).Named("daemon.budget").Info("terminal_budget_release_superseded",
			zap.Int64("session_id", rootID), zap.Int64("generation", record.Generation), zap.String("reason", reason))

		return nil
	}

	if err != nil {
		return fmt.Errorf("release terminal budget: %w", err)
	}

	return nil
}

func (s *Service) settleRootBudget(
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

func (s *Service) retainBudgetForBackground(ctx context.Context, rootID int64) (bool, error) {
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

func (s *Service) settleRunBudget(
	ctx context.Context,
	rec *sessionstore.SessionRecord,
	result session.RunResult,
	runErr error,
) error {
	if result.BudgetFired {
		record, budgetErr := s.budgets.Get(ctx, sessionRootID(rec))
		if budgetErr != nil {
			runErr = errors.Join(runErr, budgetErr)
		} else if record.State == budget.Fired {
			s.startPark(record.RootSessionID)
		}
	} else if rec.ParentID == 0 {
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

	armed := record.State == budget.Armed && record.DurationSeconds != nil
	if previous := c.timers[record.RootSessionID]; previous != nil {
		if armed && !previous.retry && previous.generation == record.Generation {
			return
		}

		previous.t.Stop()
		delete(c.timers, record.RootSessionID)
	}

	if armed {
		deadline := record.ArmedAt.Add(time.Duration(*record.DurationSeconds) * time.Second)
		c.install(record.RootSessionID, &budgetTimer{generation: record.Generation}, time.Until(deadline))
	}
}

func (c *budgetClock) retry(rootID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	previous := c.timers[rootID]
	if c.closed || (previous != nil && previous.retry) {
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

func (s *Service) refreshBudget(ctx context.Context, rootID int64) {
	if record := s.timerBudget(ctx, rootID); record != nil {
		s.clock.set(record)
	}
}

func (s *Service) timerBudget(ctx context.Context, rootID int64) *budget.Record {
	record, err := s.store.Get(ctx, rootID)
	if err == nil || errors.Is(err, budget.ErrNotFound) {
		return record
	}

	logger.Ctx(ctx).Named("daemon.budget").Warn("load_timer_budget", zap.Error(err))
	s.clock.retry(rootID)

	return nil
}

func (s *Service) budgetDue(ctx context.Context, rootID int64) {
	record := s.timerBudget(ctx, rootID)
	if record == nil {
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

		if fired {
			s.startPark(rootID)

			return
		}

		record = observed
	}

	s.clock.set(record)
}

// checkBudgetModel refuses an unpriced model for an armed cost-limited tree.
func (s *Service) checkBudgetModel(ctx context.Context, rootID int64, model string) error {
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

func (s *Service) reconcileArmedBudgets(ctx context.Context) error {
	armed, err := s.budgets.ListArmed(ctx)
	if err != nil {
		return fmt.Errorf("list armed budgets: %w", err)
	}

	for _, record := range armed {
		observed, fired, err := s.store.ObserveBudget(ctx, record.RootSessionID, time.Now().UTC(), "")
		if err != nil {
			return fmt.Errorf("reconcile budget %d: %w", record.RootSessionID, err)
		}

		if fired {
			s.parkNow(ctx, record.RootSessionID)
		} else {
			s.clock.set(observed)
		}
	}

	return nil
}

func (s *Service) parkBudgetTree(ctx context.Context, rootID int64) {
	defer s.refreshBudget(context.WithoutCancel(ctx), rootID)

	unlock, err := s.awaitParkTree(ctx, rootID)
	if err != nil {
		if errors.Is(err, errParkSuperseded) {
			s.publishIdle(rootID)
		} else if ctx.Err() == nil {
			logger.Ctx(ctx).Named("daemon.budget").Warn("lock_park_tree_failed", zap.Error(err))
		}

		return
	}

	defer unlock()

	failureEvent := "park_root_stop_failed"

	stopped, err := s.store.StopFiredBudgetRoot(ctx, rootID)
	if err == nil && stopped {
		failureEvent = "park_cleanup_failed"
		err = s.stopTreeCleanup(ctx, rootID, stopTreeOptions{preserveBackgroundProcesses: true})
	}

	if err != nil {
		logger.Ctx(ctx).Named("daemon.budget").Warn(failureEvent, zap.Error(err))
		return
	}

	if stopped {
		s.signalOutcome(ctx, rootID, orchestrator.SignalParked, 0)
	}

	s.publishIdle(rootID)
}

func (s *Service) startPark(rootID int64) {
	if s.parks.begin(rootID) && !s.life.Go("daemon.budget", func(ctx context.Context) {
		defer s.parks.end(rootID, func() { s.startPark(rootID) })

		s.parkBudgetTree(ctx, rootID)
	}) {
		s.parks.end(rootID, nil)
	}
}

func (s *Service) parkNow(ctx context.Context, rootID int64) {
	if s.parks.begin(rootID) {
		defer s.parks.end(rootID, func() { s.parkNow(ctx, rootID) })

		s.parkBudgetTree(ctx, rootID)
	}
}

func (s *Service) parkRefuses(sessionID int64, prompt string) bool {
	return s.parks.active(sessionID) && !isReadOnlyCommand(prompt) && !isEnqueueCommand(prompt)
}

func (p *parkSet) begin(rootID int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.roots == nil {
		p.roots = make(map[int64]bool)
	}

	_, exists := p.roots[rootID]
	p.roots[rootID] = exists

	return !exists
}

func (p *parkSet) end(rootID int64, rerun func()) {
	p.mu.Lock()
	pending := p.roots[rootID]
	delete(p.roots, rootID)
	p.mu.Unlock()

	if pending && rerun != nil {
		rerun()
	}
}

func (p *parkSet) active(rootID int64) bool {
	p.mu.Lock()
	_, exists := p.roots[rootID]
	p.mu.Unlock()

	return exists
}

func (s *Service) awaitParkTree(ctx context.Context, rootID int64) (func(), error) {
	for {
		record, err := s.budgets.Get(ctx, rootID)
		switch {
		case err != nil:
			return nil, fmt.Errorf("load budget before park join: %w", err)
		case record.State != budget.Fired:
			return nil, errParkSuperseded
		}

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
