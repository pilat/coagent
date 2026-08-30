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
	PersistResponse(
		ctx context.Context,
		message *sessionstore.StoredMessage,
		directReply string,
	) (messageID int64, fired, replyPublished bool, err error)
	PersistCompaction(
		ctx context.Context,
		compaction sessionstore.BudgetedCompaction,
	) (messageIDs []int64, fired bool, err error)
}
