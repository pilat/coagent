//nolint:wrapcheck // Adapter preserves budget/store sentinel errors for the session loop.; nosemgrep: semgrep.coagent-no-preamble-before-package
package daemon

import (
	"context"
	"errors"
	"fmt"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
)

const budgetParkRequested = "requested"

func (s *svc) modelHasPricing(modelID string) bool {
	for _, model := range s.modelEntries {
		if model.ID == modelID {
			return model.Pricing != nil
		}
	}

	return false
}

func sessionRootID(record *sessionstore.SessionRecord) int64 {
	if record.RootID != 0 {
		return record.RootID
	}

	return record.ID
}

func (s *svc) releaseArmedBudget(ctx context.Context, rootID int64, reason string) error {
	if s.budgetSvc == nil {
		return nil
	}

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

//nolint:wsl_v5 // Terminal budget policy remains a flat sequence of exclusive outcomes.
func (s *svc) settleRootBudget(
	ctx context.Context,
	rootID int64,
	suspended bool,
	runErr error,
	notify func(sessionevent.Notification),
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
		notify(sessionevent.Notification{
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

//nolint:wsl_v5 // Budget state gates the more expensive tree projection.
func (s *svc) retainBudgetForBackground(ctx context.Context, rootID int64) (bool, error) {
	if s.budgetSvc == nil {
		return false, nil
	}

	record, err := s.budgetSvc.Get(ctx, rootID)
	if errors.Is(err, budget.ErrNotFound) ||
		(err == nil && record.State != budget.Armed) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load budget for background projection: %w", err)
	}

	retained, err := s.hasBackgroundObligation(ctx, rootID)
	if err != nil {
		return true, fmt.Errorf("project background obligation: %w", err)
	}

	return retained, nil
}

// hasBackgroundObligation shares the ledger-first wake-source predicate at
// root-tree scope: budget retention must not release while any tree session
// could still deliver model-bound input.
func (s *svc) hasBackgroundObligation(ctx context.Context, rootID int64) (bool, error) {
	obligations, ok := s.sessionStore.(sessionstore.BackgroundObligationStore)
	if !ok {
		return false, errors.New("background obligation projection unavailable")
	}

	return obligations.HasBackgroundObligationByRoot(ctx, rootID)
}
