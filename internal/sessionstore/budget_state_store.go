package sessionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"
)

func (s *store) GetBudget(ctx context.Context, rootID int64) (*BudgetRecord, error) {
	record, err := scanBudget(s.db.QueryRowContext(ctx, budgetSelect+` WHERE root_session_id = ?`, rootID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrBudgetNotFound
	}

	if err != nil {
		return nil, fmt.Errorf("load session budget: %w", err)
	}

	return record, nil
}

func (s *store) ArmBudget(
	ctx context.Context,
	mutation BudgetMutation,
) (*BudgetRecord, *OutputCommit, error) {
	if err := validateBudgetMutation(mutation, true); err != nil {
		return nil, nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin arm budget: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	owner, baseline, err := budgetRootFacts(ctx, tx, mutation.RootSessionID)
	if err != nil {
		return nil, nil, err
	}

	if replayed, replayCommit, replayErr := replayBudgetMutation(
		ctx,
		tx,
		mutation,
		owner,
	); replayed != nil ||
		replayErr != nil {
		return replayed, replayCommit, replayErr
	}

	generation := int64(1)
	var previous int64

	err = tx.QueryRowContext(ctx, `SELECT generation FROM session_budgets WHERE root_session_id = ?`,
		mutation.RootSessionID).Scan(&previous)
	if err == nil {
		generation = previous + 1
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, fmt.Errorf("load budget generation: %w", err)
	}

	now := time.Now().UTC()

	_, err = tx.ExecContext(ctx, `INSERT INTO session_budgets
		(root_session_id, state, generation, armed_at, baseline_cost_usd, cost_limit_usd, duration_seconds)
		VALUES (?, 'armed', ?, ?, ?, ?, ?)
		ON CONFLICT(root_session_id) DO UPDATE SET state = 'armed', generation = excluded.generation,
			armed_at = excluded.armed_at, baseline_cost_usd = excluded.baseline_cost_usd,
			cost_limit_usd = excluded.cost_limit_usd, duration_seconds = excluded.duration_seconds,
			fired_at = NULL, released_at = NULL, fired_reason = '', released_reason = '',
			observed_cost_usd = NULL, park_phase = '', park_owner = ''`,
		mutation.RootSessionID, generation, now, baseline,
		nullFloat(mutation.CostLimitUSD), nullInt64(mutation.DurationSeconds))
	if err != nil {
		return nil, nil, fmt.Errorf("arm budget: %w", err)
	}

	if err := consumeActivationTx(ctx, tx, mutation, now); err != nil {
		return nil, nil, err
	}

	commit, err := insertDirectOutput(ctx, tx, mutation.RootSessionID, owner,
		mutation.ToolCallID, 0, mutation.Receipt)
	if err != nil {
		return nil, nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit arm budget: %w", err)
	}

	record, err := s.GetBudget(ctx, mutation.RootSessionID)

	return record, commit, err
}

func (s *store) ClearBudget(
	ctx context.Context,
	mutation BudgetMutation,
) (*BudgetRecord, *OutputCommit, error) {
	if err := validateBudgetMutation(mutation, false); err != nil {
		return nil, nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin clear budget: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	owner, baseline, err := budgetRootFacts(ctx, tx, mutation.RootSessionID)
	if err != nil {
		return nil, nil, err
	}

	if replayed, replayCommit, replayErr := replayBudgetMutation(
		ctx,
		tx,
		mutation,
		owner,
	); replayed != nil ||
		replayErr != nil {
		return replayed, replayCommit, replayErr
	}

	now := time.Now().UTC()

	result, err := tx.ExecContext(ctx, `UPDATE session_budgets SET state = 'released', released_at = ?,
		released_reason = 'cleared', park_owner = '' WHERE root_session_id = ?
			AND park_phase <> 'draining'`, now, mutation.RootSessionID)
	if err != nil {
		return nil, nil, fmt.Errorf("clear budget: %w", err)
	}

	affected, _ := result.RowsAffected()
	if affected == 0 {
		var phase string
		loadErr := tx.QueryRowContext(ctx, `SELECT park_phase FROM session_budgets
			WHERE root_session_id = ?`, mutation.RootSessionID).Scan(&phase)

		if loadErr == nil {
			// The row exists but the drain owns it: no external release wins the window.
			return nil, nil, ErrBudgetConflict
		}

		_, err = tx.ExecContext(ctx, `INSERT INTO session_budgets
			(root_session_id, state, generation, armed_at, baseline_cost_usd, released_at, released_reason)
			VALUES (?, 'released', 1, ?, ?, ?, 'cleared')`, mutation.RootSessionID, now, baseline, now)
		if err != nil {
			return nil, nil, fmt.Errorf("record absent budget clear: %w", err)
		}
	}

	if err := consumeActivationTx(ctx, tx, mutation, now); err != nil {
		return nil, nil, err
	}

	commit, err := insertDirectOutput(ctx, tx, mutation.RootSessionID, owner,
		mutation.ToolCallID, 0, mutation.Receipt)
	if err != nil {
		return nil, nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit clear budget: %w", err)
	}

	record, err := s.GetBudget(ctx, mutation.RootSessionID)

	return record, commit, err
}

func validateBudgetMutation(mutation BudgetMutation, arm bool) error {
	if mutation.RootSessionID <= 0 || mutation.InputID <= 0 || mutation.ToolID == "" ||
		mutation.Command == "" || mutation.ToolCallID == "" || mutation.Receipt == "" {
		return ErrBudgetConflict
	}

	if arm == (mutation.CostLimitUSD == nil && mutation.DurationSeconds == nil) {
		return ErrBudgetConflict
	}

	if mutation.CostLimitUSD != nil && (*mutation.CostLimitUSD <= 0 || *mutation.CostLimitUSD > 1_000_000 ||
		math.IsNaN(*mutation.CostLimitUSD) || math.IsInf(*mutation.CostLimitUSD, 0)) {
		return ErrBudgetConflict
	}

	if mutation.DurationSeconds != nil && (*mutation.DurationSeconds < 60 || *mutation.DurationSeconds > 31_536_000) {
		return ErrBudgetConflict
	}

	return nil
}

func consumeActivationTx(ctx context.Context, tx *sql.Tx, mutation BudgetMutation, now time.Time) error {
	result, err := tx.ExecContext(ctx, `UPDATE session_tool_activations
		SET state = 'consumed', tool_call_id = ?, resolved_at = ?
		WHERE input_id = ? AND session_id = ? AND tool_id = ? AND command = ? AND state = 'pending'`,
		mutation.ToolCallID, now, mutation.InputID, mutation.RootSessionID, mutation.ToolID, mutation.Command)
	if err != nil {
		return fmt.Errorf("consume budget activation: %w", err)
	}

	if err := requireActivationChanged(result); err != nil {
		return ErrBudgetConflict
	}

	return nil
}

func replayBudgetMutation(
	ctx context.Context,
	tx *sql.Tx,
	mutation BudgetMutation,
	owner string,
) (*BudgetRecord, *OutputCommit, error) {
	var state, callID, toolID, command string

	err := tx.QueryRowContext(ctx, `SELECT state, COALESCE(tool_call_id, ''), tool_id, command
		FROM session_tool_activations WHERE input_id = ? AND session_id = ?`,
		mutation.InputID, mutation.RootSessionID).Scan(&state, &callID, &toolID, &command)
	if errors.Is(err, sql.ErrNoRows) || state == "pending" {
		return nil, nil, nil
	}

	if err != nil {
		return nil, nil, fmt.Errorf("load budget activation replay: %w", err)
	}

	if state != "consumed" || callID != mutation.ToolCallID || toolID != mutation.ToolID ||
		command != mutation.Command {
		return nil, nil, ErrBudgetConflict
	}

	record, err := scanBudget(
		tx.QueryRowContext(ctx, budgetSelect+` WHERE root_session_id = ?`, mutation.RootSessionID),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("load replayed budget: %w", err)
	}

	commit, err := insertDirectOutput(ctx, tx, mutation.RootSessionID, owner,
		mutation.ToolCallID, 0, mutation.Receipt)
	if err != nil {
		return nil, nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit budget replay: %w", err)
	}

	return record, commit, nil
}

func budgetRootFacts(ctx context.Context, tx *sql.Tx, rootID int64) (string, float64, error) {
	owner, err := outputOwner(ctx, tx, rootID)
	if err != nil {
		return "", 0, err
	}
	var parent int64
	var cost float64

	err = tx.QueryRowContext(ctx, `SELECT sessions.parent_id, COALESCE(SUM(messages.cost_usd), 0)
		FROM sessions LEFT JOIN sessions tree ON tree.id = sessions.id OR tree.root_id = sessions.id
		LEFT JOIN messages ON messages.session_id = tree.id WHERE sessions.id = ? GROUP BY sessions.id`, rootID).
		Scan(&parent, &cost)
	if err != nil || parent != 0 || math.IsNaN(cost) || math.IsInf(cost, 0) || cost < 0 {
		return "", 0, ErrBudgetConflict
	}

	return owner, cost, nil
}

func nullFloat(value *float64) any {
	if value == nil {
		return nil
	}

	return *value
}

func nullInt64(value *int64) any {
	if value == nil {
		return nil
	}

	return *value
}
