//nolint:wrapcheck // Store row iteration errors retain SQLite details.; nosemgrep: semgrep.coagent-no-preamble-before-package
package sessionstore

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type BudgetState string

const (
	BudgetArmed    BudgetState = "armed"
	BudgetFired    BudgetState = "fired"
	BudgetReleased BudgetState = "released"
)

const budgetParkRequestedState = "requested"

var (
	ErrBudgetNotFound = errors.New("session budget not found")
	ErrBudgetConflict = errors.New("session budget conflict")
)

type BudgetRecord struct {
	RootSessionID   int64
	State           BudgetState
	Generation      int64
	ArmedAt         time.Time
	BaselineCostUSD float64
	CostLimitUSD    *float64
	DurationSeconds *int64
	FiredAt         *time.Time
	ReleasedAt      *time.Time
	FiredReason     string
	ReleasedReason  string
	ObservedCostUSD *float64
	ParkPhase       string
	ParkOwner       string
}

type BudgetMutation struct {
	RootSessionID   int64
	InputID         int64
	ToolID          string
	Command         string
	ToolCallID      string
	CostLimitUSD    *float64
	DurationSeconds *int64
	Receipt         string
}

type BudgetStore interface {
	GetBudget(ctx context.Context, rootID int64) (*BudgetRecord, error)
	ArmBudget(ctx context.Context, mutation BudgetMutation) (*BudgetRecord, *OutputCommit, error)
	ClearBudget(ctx context.Context, mutation BudgetMutation) (*BudgetRecord, *OutputCommit, error)
	FireBudget(ctx context.Context, rootID, generation int64, reason string, observedCost float64,
		content string) (*BudgetRecord, *OutputCommit, error)
	ObserveBudget(
		ctx context.Context,
		rootID int64,
		observedAt time.Time,
		assistantText string,
	) (*BudgetRecord, bool, error)
	ReleaseBudget(ctx context.Context, rootID, generation int64, reason string) (*BudgetRecord, error)
	BeginBudgetDrain(ctx context.Context, rootID, generation int64, owner string) (*BudgetRecord, error)
	MarkBudgetParked(ctx context.Context, rootID, generation int64, owner string) (*BudgetRecord, error)
	ListPendingBudgetParks(ctx context.Context) ([]*BudgetRecord, error)
	ListArmedBudgets(ctx context.Context) ([]*BudgetRecord, error)
}

var _ BudgetStore = (*store)(nil)

func (s *store) ListArmedBudgets(ctx context.Context) ([]*BudgetRecord, error) {
	return s.listBudgets(ctx, ` WHERE state = 'armed' ORDER BY root_session_id`)
}

func (s *store) ListPendingBudgetParks(ctx context.Context) ([]*BudgetRecord, error) {
	return s.listBudgets(ctx, ` WHERE state = 'fired'
		AND park_phase IN ('requested', 'draining') ORDER BY root_session_id`)
}

//nolint:wsl_v5 // Row iteration stays grouped with its scan and append.
func (s *store) listBudgets(ctx context.Context, suffix string) ([]*BudgetRecord, error) {
	rows, err := s.db.QueryContext(ctx, budgetSelect+suffix)
	if err != nil {
		return nil, fmt.Errorf("list budgets: %w", err)
	}
	defer rows.Close()
	var records []*BudgetRecord

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
) (*BudgetRecord, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE session_budgets SET park_phase = 'draining'
		WHERE root_session_id = ? AND generation = ? AND state = 'fired'
			AND park_phase = 'requested' AND park_owner = ?`, rootID, generation, owner)
	if err != nil {
		return nil, fmt.Errorf("begin budget drain: %w", err)
	}
	if err := requireActivationChanged(result); err != nil {
		return nil, ErrBudgetConflict
	}

	return s.GetBudget(ctx, rootID)
}

//nolint:wsl_v5 // CAS mutation and validation are one store operation.
func (s *store) MarkBudgetParked(
	ctx context.Context,
	rootID, generation int64,
	owner string,
) (*BudgetRecord, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE session_budgets
		SET park_phase = 'parked', park_owner = ''
		WHERE root_session_id = ? AND generation = ? AND state = 'fired'
			AND park_phase = 'draining' AND park_owner = ?`, rootID, generation, owner)
	if err != nil {
		return nil, fmt.Errorf("mark budget parked: %w", err)
	}
	if err := requireActivationChanged(result); err != nil {
		return nil, ErrBudgetConflict
	}

	return s.GetBudget(ctx, rootID)
}
