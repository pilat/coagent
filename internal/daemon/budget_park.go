package daemon

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionstore"
)

func (s *svc) parkBudgetTree(ctx context.Context, record *sessionstore.BudgetRecord) {
	if record == nil || record.State != sessionstore.BudgetFired || record.ParkOwner == "" {
		return
	}

	owner := record.ParkOwner
	if record.ParkPhase == budgetParkRequested {
		_, err := s.budgetSvc.BeginDrain(ctx, record.RootSessionID, record.Generation, owner)
		if errors.Is(err, sessionstore.ErrBudgetConflict) {
			return
		}

		if err != nil {
			logger.Ctx(ctx).Named("daemon.budget").Warn("begin_drain_failed", zap.Error(err))
			return
		}
	}

	for s.treeHasActiveLoop(ctx, record.RootSessionID) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}

	if err := s.stopTreeCleanup(ctx, record.RootSessionID); err != nil {
		logger.Ctx(ctx).Named("daemon.budget").Warn("park_cleanup_failed", zap.Error(err))
		return
	}

	if _, err := s.budgetSvc.MarkParked(ctx, record.RootSessionID, record.Generation, owner); err != nil {
		logger.Ctx(ctx).Named("daemon.budget").Warn("mark_parked_failed", zap.Error(err))
		return
	}

	s.reconcileLatestReadiness(ctx, record.RootSessionID)
}

func (s *svc) startBudgetPark(record *sessionstore.BudgetRecord) {
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
	records, err := s.sessionStore.ListAllSessions(ctx)
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
