//nolint:wrapcheck // Store row iteration errors retain SQLite details.; nosemgrep: semgrep.coagent-no-preamble-before-package
package sessionstore

import (
	"context"
	"fmt"
	"time"

	"github.com/pilat/coagent/internal/budget"
)

const budgetParkRequestedState = "requested"

type BudgetStore interface {
	Get(ctx context.Context, rootID int64) (*budget.Record, error)
	Arm(ctx context.Context, mutation budget.Mutation) (*budget.Record, error)
	Clear(ctx context.Context, mutation budget.Mutation) (*budget.Record, error)
	FireBudget(ctx context.Context, rootID, generation int64, reason string, observedCost float64,
		content string) (*budget.Record, *OutputCommit, error)
	ObserveBudget(
		ctx context.Context,
		rootID int64,
		observedAt time.Time,
		assistantText string,
	) (*budget.Record, bool, error)
	ReleaseBudget(ctx context.Context, rootID, generation int64, reason string) (*budget.Record, error)
	BeginBudgetDrain(ctx context.Context, rootID, generation int64, owner string) (*budget.Record, error)
	MarkBudgetParked(ctx context.Context, rootID, generation int64, owner string) (*budget.Record, error)
	ListPendingBudgetParks(ctx context.Context) ([]*budget.Record, error)
	ListArmedBudgets(ctx context.Context) ([]*budget.Record, error)
}

var _ BudgetStore = (*store)(nil)

func (s *store) ListArmedBudgets(ctx context.Context) ([]*budget.Record, error) {
	return s.listBudgets(ctx, ` WHERE state = 'armed' ORDER BY root_session_id`)
}

func (s *store) ListPendingBudgetParks(ctx context.Context) ([]*budget.Record, error) {
	return s.listBudgets(ctx, ` WHERE state = 'fired'
		AND park_phase IN ('requested', 'draining') ORDER BY root_session_id`)
}

//nolint:wsl_v5 // Row iteration stays grouped with its scan and append.
func (s *store) listBudgets(ctx context.Context, suffix string) ([]*budget.Record, error) {
	rows, err := s.db.QueryContext(ctx, budgetSelect+suffix)
	if err != nil {
		return nil, fmt.Errorf("list budgets: %w", err)
	}
	defer rows.Close()
	var records []*budget.Record

	for rows.Next() {
		record, scanErr := scanBudget(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan budget: %w", scanErr)
		}
		records = append(records, record)
	}

	return records, rows.Err()
}

//nolint:wsl_v5 // CAS mutation and validation are one store operation.
func (s *store) BeginBudgetDrain(
	ctx context.Context,
	rootID, generation int64,
	owner string,
) (*budget.Record, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE session_budgets SET park_phase = 'draining'
		WHERE root_session_id = ? AND generation = ? AND state = 'fired'
			AND park_phase = 'requested' AND park_owner = ?`, rootID, generation, owner)
	if err != nil {
		return nil, fmt.Errorf("begin budget drain: %w", err)
	}
	if err := requireActivationChanged(result); err != nil {
		return nil, budget.ErrConflict
	}

	return s.Get(ctx, rootID)
}

//nolint:wsl_v5 // CAS mutation and validation are one store operation.
func (s *store) MarkBudgetParked(
	ctx context.Context,
	rootID, generation int64,
	owner string,
) (*budget.Record, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE session_budgets
		SET park_phase = 'parked', park_owner = ''
		WHERE root_session_id = ? AND generation = ? AND state = 'fired'
			AND park_phase = 'draining' AND park_owner = ?`, rootID, generation, owner)
	if err != nil {
		return nil, fmt.Errorf("mark budget parked: %w", err)
	}
	if err := requireActivationChanged(result); err != nil {
		return nil, budget.ErrConflict
	}

	return s.Get(ctx, rootID)
}
