//nolint:wrapcheck // Store scanners attach operation context at their callers.; nosemgrep: semgrep.coagent-no-preamble-before-package
package sessionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"
)

const budgetSelect = `SELECT root_session_id, state, generation, armed_at, baseline_cost_usd,
	cost_limit_usd, duration_seconds, fired_at, released_at, fired_reason, released_reason,
	observed_cost_usd, park_phase, park_owner FROM session_budgets`

func (s *store) FireBudget(
	ctx context.Context,
	rootID, generation int64,
	reason string,
	observedCost float64,
	content string,
) (*BudgetRecord, *OutputCommit, error) {
	if rootID <= 0 || generation <= 0 || (reason != "cost" && reason != "duration") || content == "" ||
		observedCost < 0 || math.IsNaN(observedCost) || math.IsInf(observedCost, 0) {
		return nil, nil, ErrBudgetConflict
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin fire budget: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	owner, err := outputOwner(ctx, tx, rootID)
	if err != nil {
		return nil, nil, err
	}

	record, commit, err := fireBudgetTx(ctx, tx, rootID, generation, reason, observedCost, content, owner)
	if err != nil {
		return nil, nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit fire budget: %w", err)
	}

	if commit.Existing {
		return record, commit, nil
	}

	fired, err := s.GetBudget(ctx, rootID)

	return fired, commit, err
}

// ObserveBudget is the single-transaction admission observation: it reads the
// budget, computes the crossing from the in-transaction tree cost and, on a
// crossing, fires within the same writer serialization — no read-then-CAS gap.
func (s *store) ObserveBudget(
	ctx context.Context,
	rootID int64,
	observedAt time.Time,
	assistantText string,
) (*BudgetRecord, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("begin budget observation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	record, err := scanBudget(tx.QueryRowContext(ctx, budgetSelect+` WHERE root_session_id = ?`, rootID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}

	if err != nil {
		return nil, false, fmt.Errorf("load observed budget: %w", err)
	}

	if record.State != BudgetArmed {
		return record, record.State == BudgetFired, nil
	}

	reason, delta, err := budgetCrossing(ctx, tx, record, observedAt)
	if err != nil {
		return nil, false, err
	}

	if reason == "" {
		return record, false, nil
	}

	content := fmt.Sprintf(
		"Budget checkpoint reached (%s). Persisted cost: $%.6f. The limiter is no longer armed.", reason, delta,
	)
	if assistantText != "" {
		content = assistantText + "\n\n" + content
	}

	owner, err := outputOwner(ctx, tx, rootID)
	if err != nil {
		return nil, false, err
	}

	fired, _, err := fireBudgetTx(ctx, tx, rootID, record.Generation, reason, delta, content, owner)
	if err != nil {
		return nil, false, err
	}

	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("commit budget observation: %w", err)
	}

	if fired == nil {
		fired, err = s.GetBudget(ctx, rootID)
		if err != nil {
			return nil, false, err
		}
	}

	return fired, true, nil
}

// fireBudgetTx CASes armed→fired and inserts the checkpoint inside the caller's
// transaction; a duplicate observer receives the existing generation instead of
// a second fire. The caller owns the commit.
func fireBudgetTx(
	ctx context.Context,
	tx *sql.Tx,
	rootID, generation int64,
	reason string,
	observedCost float64,
	content, owner string,
) (*BudgetRecord, *OutputCommit, error) {
	now := time.Now().UTC()
	parkOwner := fmt.Sprintf("budget:%d:%d", rootID, generation)

	result, err := tx.ExecContext(ctx, `UPDATE session_budgets SET state = 'fired', fired_at = ?,
		fired_reason = ?, observed_cost_usd = ?, park_phase = 'requested', park_owner = ?
		WHERE root_session_id = ? AND generation = ? AND state = 'armed'`,
		now, reason, observedCost, parkOwner, rootID, generation)
	if err != nil {
		return nil, nil, fmt.Errorf("fire budget: %w", err)
	}

	affected, _ := result.RowsAffected()
	if affected == 0 {
		existing, loadErr := scanBudget(tx.QueryRowContext(ctx, budgetSelect+
			` WHERE root_session_id = ? AND generation = ?`, rootID, generation))
		if loadErr != nil || existing.State != BudgetFired {
			return nil, nil, ErrBudgetConflict
		}

		commit, commitErr := budgetOutputByKey(ctx, tx, rootID, generation)
		if commitErr != nil {
			return nil, nil, commitErr
		}

		return existing, commit, nil
	}

	key := fmt.Sprintf("budget:%d:checkpoint", generation)

	commit, err := insertMessageOutput(ctx, tx, rootID, owner, content, key, now, true)
	if err != nil {
		return nil, nil, err
	}

	return nil, commit, nil
}

func (s *store) ReleaseBudget(
	ctx context.Context,
	rootID, generation int64,
	reason string,
) (*BudgetRecord, error) {
	if !validBudgetReleaseReason(reason) {
		return nil, ErrBudgetConflict
	}

	result, err := s.db.ExecContext(ctx, `UPDATE session_budgets SET state = 'released', released_at = ?,
		released_reason = ?, park_owner = '' WHERE root_session_id = ? AND generation = ?
			AND state IN ('armed', 'fired') AND park_phase <> 'draining'`,
		time.Now().UTC(), reason, rootID, generation)
	if err != nil {
		return nil, fmt.Errorf("release budget: %w", err)
	}

	affected, _ := result.RowsAffected()
	if affected == 0 {
		existing, loadErr := s.GetBudget(ctx, rootID)
		if loadErr == nil && existing.Generation == generation && existing.State == BudgetReleased &&
			existing.ReleasedReason == reason {
			return existing, nil
		}

		return nil, ErrBudgetConflict
	}

	return s.GetBudget(ctx, rootID)
}

func scanBudget(row interface{ Scan(...any) error }) (*BudgetRecord, error) {
	var record BudgetRecord
	var cost sql.NullFloat64
	var duration sql.NullInt64
	var firedAt, releasedAt sql.NullTime
	var observed sql.NullFloat64

	err := row.Scan(&record.RootSessionID, &record.State, &record.Generation, &record.ArmedAt,
		&record.BaselineCostUSD, &cost, &duration, &firedAt, &releasedAt, &record.FiredReason,
		&record.ReleasedReason, &observed, &record.ParkPhase, &record.ParkOwner)
	if err != nil {
		return nil, err
	}

	if cost.Valid {
		record.CostLimitUSD = &cost.Float64
	}

	if duration.Valid {
		record.DurationSeconds = &duration.Int64
	}

	if firedAt.Valid {
		record.FiredAt = &firedAt.Time
	}

	if releasedAt.Valid {
		record.ReleasedAt = &releasedAt.Time
	}

	if observed.Valid {
		record.ObservedCostUSD = &observed.Float64
	}

	return &record, nil
}

func budgetOutputByKey(
	ctx context.Context,
	tx *sql.Tx,
	rootID, generation int64,
) (*OutputCommit, error) {
	var id int64
	var owner string

	err := tx.QueryRowContext(ctx, `SELECT id, json_extract(attributes, '$.manager_id')
		FROM session_outbox WHERE session_id = ? AND source_key = ?`,
		rootID, fmt.Sprintf("budget:%d:checkpoint", generation)).Scan(&id, &owner)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOutputConflict
	}

	if err != nil {
		return nil, fmt.Errorf("load budget checkpoint output: %w", err)
	}

	return &OutputCommit{OutputID: id, OwnerID: owner, Existing: true}, nil
}

func validBudgetReleaseReason(reason string) bool {
	switch reason {
	case "cleared", "resumed", "completed", "error", "stopped", "killed", "replaced":
		return true
	default:
		return false
	}
}
