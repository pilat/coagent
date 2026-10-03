package session

import (
	"context"

	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/transcript"
)

// Store is the persistence view required by session.
type Store interface {
	Commit(context.Context, sessionstore.Commit) (*sessionstore.CommitResult, error)
	ListPending(context.Context, int64) ([]*sessionstore.InboxInput, error)
	PendingActivation(context.Context, int64) (*sessionstore.ToolActivation, error)

	HasBackgroundWakeSource(context.Context, int64) (bool, error)
	LoadActiveMessages(context.Context, int64) ([]*transcript.Message, error)
	LoadCompletionCheckState(context.Context, int64) (*sessionstore.CompletionCheckState, error)
	HasOutstandingResponseRecovery(context.Context, int64) (bool, error)
	GetChildSessionStats(context.Context, int64) (int, int, error)
	GetSessionTreeUsage(context.Context, int64) (int, int, float64, error)
	LookupRead(ctx context.Context, sessionID int64, path string) (sessionstore.FileReadRecord, bool, error)
	RecordRead(ctx context.Context, sessionID int64, path string, record sessionstore.FileReadRecord) error
}
