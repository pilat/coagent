package session

import (
	"context"
	"errors"
	"time"

	"github.com/pilat/coagent/internal/sessionstore"
)

var ErrBudgetCheckpoint = errors.New("budget checkpoint fired")

type BudgetGate interface {
	Admit(ctx context.Context, now time.Time) error
	Observe(ctx context.Context) (fired bool, err error)
	PersistRejectedResponse(
		ctx context.Context,
		rejection sessionstore.RejectedResponse,
	) (*sessionstore.RejectedResponseResult, error)
	PersistCompaction(
		ctx context.Context,
		compaction sessionstore.BudgetedCompaction,
	) (messageIDs []int64, fired bool, err error)
	// BudgetFired schedules the host park from a committed response verdict.
	BudgetFired(record *sessionstore.BudgetRecord)
}
