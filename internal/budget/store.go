package budget

import (
	"context"
	"time"
)

type Store interface {
	Get(context.Context, int64) (*Record, error)
	Arm(context.Context, Mutation) (*Record, error)
	Clear(context.Context, Mutation) (*Record, error)
}

type PolicyStore interface {
	Store
	ObserveBudget(context.Context, int64, time.Time, string) (*Record, bool, error)
	ReleaseBudget(context.Context, int64, int64, string) (*Record, error)
	BeginBudgetDrain(context.Context, int64, int64, string) (*Record, error)
	MarkBudgetParked(context.Context, int64, int64, string) (*Record, error)
	ListPendingBudgetParks(context.Context) ([]*Record, error)
	ListArmedBudgets(context.Context) ([]*Record, error)
}
