package budget

import (
	"context"
	"time"
)

type Store interface {
	GetBudget(context.Context, int64) (*Record, error)
	ArmBudget(context.Context, Mutation) (*Record, error)
	ClearBudget(context.Context, Mutation) (*Record, error)
	ObserveBudget(context.Context, int64, time.Time, string) (*Record, bool, error)
	ReleaseBudget(context.Context, int64, int64, string) (*Record, error)
	BeginBudgetDrain(context.Context, int64, int64, string) (*Record, error)
	MarkBudgetParked(context.Context, int64, int64, string) (*Record, error)
	ListPendingBudgetParks(context.Context) ([]*Record, error)
	ListArmedBudgets(context.Context) ([]*Record, error)
}
