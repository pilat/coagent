package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/logger"
)

type budgetDeadline struct {
	generation int64
	timer      *time.Timer
	cancel     chan struct{}
	park       *budget.Record
	refresh    bool
}

func (s *svc) reconcileArmedBudgets(ctx context.Context) error {
	armed, err := s.budgetSvc.ListArmed(ctx)
	if err != nil {
		return fmt.Errorf("list armed budgets: %w", err)
	}

	for _, record := range armed {
		observed, fired, err := s.store.ObserveBudget(ctx, record.RootSessionID, time.Now().UTC(), "")
		if err != nil {
			return fmt.Errorf("reconcile budget %d: %w", record.RootSessionID, err)
		}

		if !fired {
			s.setBudgetTimer(observed)
		}
	}

	return nil
}

func (s *svc) refreshBudgetTimer(ctx context.Context, rootID int64) {
	if ctx.Err() != nil {
		return
	}

	record, err := s.store.Get(ctx, rootID)
	if errors.Is(err, budget.ErrNotFound) {
		return
	}

	if err != nil {
		logger.Ctx(ctx).Named("daemon.budget").Warn("load_timer_budget", zap.Error(err))
		s.scheduleBudgetRefresh(rootID)

		return
	}

	s.setBudgetTimer(record)
}

func (s *svc) setBudgetTimer(record *budget.Record) {
	s.budgetTimerMu.Lock()
	defer s.budgetTimerMu.Unlock()

	if s.shuttingDown.Load() {
		return
	}

	previous := s.budgetTimers[record.RootSessionID]
	armed := record.State == budget.Armed && record.DurationSeconds != nil

	parking := record.State == budget.Fired && (record.ParkPhase == "requested" || record.ParkPhase == "draining")
	if previous != nil && (armed || parking) && previous.generation == record.Generation &&
		(previous.park != nil) == parking {
		return
	}

	if previous != nil {
		previous.timer.Stop()
		close(previous.cancel)
		delete(s.budgetTimers, record.RootSessionID)
	}

	if !armed && !parking {
		return
	}

	deadline := time.Now().Add(time.Second)
	if armed {
		deadline = record.ArmedAt.Add(time.Duration(*record.DurationSeconds) * time.Second)
	}

	pending := &budgetDeadline{
		generation: record.Generation,
		timer:      time.NewTimer(time.Until(deadline)),
		cancel:     make(chan struct{}),
	}
	if parking {
		pending.park = record
	}

	s.budgetTimers[record.RootSessionID] = pending
	s.budgetWG.Go(func() { s.waitBudgetDeadline(record.RootSessionID, pending) })
}

func (s *svc) waitBudgetDeadline(rootID int64, pending *budgetDeadline) {
	defer pending.timer.Stop()
	defer s.finishBudgetTimer(rootID, pending)

	select {
	case <-s.budgetCtx.Done():
		return
	case <-pending.cancel:
		return
	case <-pending.timer.C:
	}

	if pending.refresh {
		return
	}

	if pending.park != nil {
		s.parkBudgetTree(s.budgetCtx, pending.park)
		return
	}

	for attempt := range 3 {
		record, fired, err := s.store.ObserveBudget(s.budgetCtx, rootID, time.Now().UTC(), "")
		if err == nil {
			if fired && record.ParkPhase == budgetParkRequested {
				s.startBudgetPark(record)
			}

			return
		}

		logger.Ctx(s.budgetCtx).
			Named("daemon.budget").
			Warn("deadline_observation_failed", zap.Int("attempt", attempt+1), zap.Error(err))

		if attempt == 2 {
			return
		}

		pending.timer.Reset(time.Second)

		select {
		case <-s.budgetCtx.Done():
			return
		case <-pending.cancel:
			return
		case <-pending.timer.C:
		}
	}
}

func (s *svc) finishBudgetTimer(rootID int64, pending *budgetDeadline) {
	s.budgetTimerMu.Lock()

	current := s.budgetTimers[rootID] == pending
	if current {
		delete(s.budgetTimers, rootID)
	}
	s.budgetTimerMu.Unlock()

	if current && s.budgetCtx.Err() == nil {
		s.refreshBudgetTimer(s.budgetCtx, rootID)
	}
}

func (s *svc) scheduleBudgetRefresh(rootID int64) {
	s.budgetTimerMu.Lock()
	defer s.budgetTimerMu.Unlock()

	if s.shuttingDown.Load() {
		return
	}

	previous := s.budgetTimers[rootID]
	if previous != nil && previous.refresh {
		return
	}

	if previous != nil {
		previous.timer.Stop()
		close(previous.cancel)
	}

	pending := &budgetDeadline{timer: time.NewTimer(time.Second), cancel: make(chan struct{}), refresh: true}
	s.budgetTimers[rootID] = pending
	s.budgetWG.Go(func() { s.waitBudgetDeadline(rootID, pending) })
}
