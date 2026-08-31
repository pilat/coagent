package progressruntime

import (
	"context"
	"fmt"
	"time"
)

func (r *runtime) reconcileArmedBudgets(ctx context.Context) error {
	if r.budgetSvc == nil {
		return nil
	}

	armed, err := r.budgetSvc.ListArmed(ctx)
	if err != nil {
		return fmt.Errorf("list armed budgets: %w", err)
	}

	now := time.Now().UTC()

	for _, budgetRecord := range armed {
		facts, captureErr := r.sessionStore.CaptureProgress(ctx, budgetRecord.RootSessionID)
		if captureErr != nil {
			return fmt.Errorf("capture budget progress for session %d: %w", budgetRecord.RootSessionID, captureErr)
		}

		record, fired, observeErr := r.budgetSvc.Observe(
			ctx, budgetRecord.RootSessionID, facts.CostUSD, now, "",
		)
		if observeErr != nil {
			return fmt.Errorf("reconcile budget for session %d: %w", budgetRecord.RootSessionID, observeErr)
		}

		if fired {
			r.startBudgetPark(record)
		}
	}

	return nil
}
