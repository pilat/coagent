package sessionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func commitRejectedBudget(
	ctx context.Context,
	tx *sql.Tx,
	rejection RejectedResponse,
	now time.Time,
) (*BudgetRecord, *OutputCommit, bool, error) {
	record, err := scanBudget(tx.QueryRowContext(ctx, budgetSelect+` WHERE root_session_id = ?`, rejection.RootID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, false, nil
	}

	if err != nil {
		return nil, nil, false, fmt.Errorf("load rejected response budget: %w", err)
	}

	if record.State == BudgetFired {
		return record, nil, true, nil
	}

	if record.State != BudgetArmed {
		return record, nil, false, nil
	}

	reason, delta, err := budgetCrossing(ctx, tx, record, now)
	if err != nil || reason == "" {
		return record, nil, false, err
	}

	output, err := fireRejectedBudget(ctx, tx, rejection.RootID, record, reason, delta, now)
	if err != nil {
		return nil, nil, false, err
	}

	return record, output, true, nil
}

func fireRejectedBudget(
	ctx context.Context,
	tx *sql.Tx,
	rootID int64,
	record *BudgetRecord,
	reason string,
	delta float64,
	now time.Time,
) (*OutputCommit, error) {
	parkOwner := fmt.Sprintf("budget:%d:%d", rootID, record.Generation)

	result, err := tx.ExecContext(ctx, `UPDATE session_budgets SET state = 'fired', fired_at = ?,
		fired_reason = ?, observed_cost_usd = ?, park_phase = 'requested', park_owner = ?
		WHERE root_session_id = ? AND generation = ? AND state = 'armed'`,
		now, reason, delta, parkOwner, rootID, record.Generation)
	if err != nil {
		return nil, fmt.Errorf("fire rejected response budget: %w", err)
	}

	if err := requireActivationChanged(result); err != nil {
		return nil, ErrBudgetConflict
	}

	owner, err := outputOwner(ctx, tx, rootID)
	if err != nil {
		return nil, err
	}

	content := fmt.Sprintf(
		"Budget checkpoint reached (%s). Persisted cost: $%.6f. The limiter is no longer armed.", reason, delta,
	)

	output, err := insertMessageOutput(ctx, tx, rootID, owner, content,
		fmt.Sprintf("budget:%d:checkpoint", record.Generation), now, true)
	if err != nil {
		return nil, err
	}

	record.State = BudgetFired
	record.FiredReason = reason
	record.FiredAt = &now
	record.ObservedCostUSD = &delta
	record.ParkPhase = budgetParkRequestedState
	record.ParkOwner = parkOwner

	return output, nil
}
