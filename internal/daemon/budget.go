package daemon

import (
	"context"
	"errors"
	"fmt"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
)

const budgetParkRequested = "requested"

func sessionRootID(record *sessionstore.SessionRecord) int64 {
	if record.RootID != 0 {
		return record.RootID
	}

	return record.ID
}

func (s *svc) releaseArmedBudget(ctx context.Context, rootID int64, reason string) error {
	record, err := s.budgetSvc.Get(ctx, rootID)
	if errors.Is(err, budget.ErrNotFound) ||
		(err == nil && record.State != budget.Armed) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("load budget for terminal release: %w", err)
	}

	if _, err := s.budgetSvc.Release(ctx, rootID, record.Generation, reason); err != nil {
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
	record, err := s.budgetSvc.Get(ctx, rootID)
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
		record, budgetErr := s.budgetSvc.Get(ctx, sessionRootID(rec))
		if budgetErr != nil {
			runErr = errors.Join(runErr, budgetErr)
		} else if record.ParkPhase == budgetParkRequested {
			s.startBudgetPark(record)
		}
	}

	if rec.ParentID == 0 && !result.BudgetFired {
		runErr = errors.Join(runErr, s.settleRootBudget(ctx, rec.ID, result.Suspended, runErr))
	}

	return runErr
}
