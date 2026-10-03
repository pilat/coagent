package daemon

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/logger"
)

func (s *svc) parkBudgetTree(ctx context.Context, record *budget.Record) {
	if record == nil || record.State != budget.Fired || record.ParkOwner == "" {
		return
	}
	defer s.refreshBudgetTimer(s.budgetCtx, record.RootSessionID)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	owner := record.ParkOwner
	if record.ParkPhase == budgetParkRequested {
		_, err := s.budgetSvc.BeginDrain(ctx, record.RootSessionID, record.Generation, owner)
		if errors.Is(err, budget.ErrConflict) {
			return
		}

		if err != nil {
			logger.Ctx(ctx).Named("daemon.budget").Warn("begin_drain_failed", zap.Error(err))
			return
		}
	}

	var unlock func()

	for {
		for s.treeHasActiveLoop(ctx, record.RootSessionID) {
			select {
			case <-ctx.Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
		}

		var err error

		unlock, err = s.lockSessionTree(ctx, record.RootSessionID)
		if err != nil {
			logger.Ctx(ctx).Named("daemon.budget").Warn("lock_park_tree_failed", zap.Error(err))
			return
		}

		if s.treeHasActiveLoop(ctx, record.RootSessionID) {
			unlock()
			continue
		}

		break
	}

	defer unlock()
	current, err := s.budgetSvc.Get(ctx, record.RootSessionID)
	if err != nil {
		logger.Ctx(ctx).Named("daemon.budget").Warn("load_park_generation", zap.Error(err))
		return
	}
	if current.Generation != record.Generation || current.State != budget.Fired || current.ParkOwner != owner ||
		(current.ParkPhase != "requested" && current.ParkPhase != "draining") {
		return
	}

	if err := s.stopTreeCleanup(ctx, record.RootSessionID, stopTreeOptions{
		preserveBackgroundProcesses: true,
	}); err != nil {
		logger.Ctx(ctx).Named("daemon.budget").Warn("park_cleanup_failed", zap.Error(err))
		return
	}

	if _, err := s.budgetSvc.MarkParked(ctx, record.RootSessionID, record.Generation, owner); err != nil {
		logger.Ctx(ctx).Named("daemon.budget").Warn("mark_parked_failed", zap.Error(err))
		return
	}

	s.reconcileLatestReadiness(ctx, record.RootSessionID)
}

func (s *svc) startBudgetPark(record *budget.Record) {
	s.budgetTimerMu.Lock()
	defer s.budgetTimerMu.Unlock()
	if record == nil || s.shuttingDown.Load() {
		return
	}

	s.budgetWG.Go(func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				logger.Ctx(s.budgetCtx).Named("daemon.budget").Error(
					"park_panic", zap.Any("panic", recovered), zap.Stack("stack"),
				)
			}
		}()

		s.parkBudgetTree(s.budgetCtx, record)
	})
}

func (s *svc) treeHasActiveLoop(ctx context.Context, rootID int64) bool {
	records, err := s.store.ListAllSessions(ctx)
	if err != nil {
		return ctx.Err() == nil
	}

	for _, record := range records {
		if (record.ID == rootID || record.RootID == rootID) && s.HasActiveLoop(record.ID) {
			return true
		}
	}

	return false
}
