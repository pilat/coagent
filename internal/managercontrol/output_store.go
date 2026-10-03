package managercontrol

import (
	"context"
	"time"

	"github.com/pilat/coagent/internal/sessionstore"
)

// OutputStore is the persistence view required by managercontrol.
type OutputStore interface {
	EnqueueOutput(ctx context.Context, draft sessionstore.OutputDraft) (*sessionstore.OutputCommit, error)
	BindManager(ctx context.Context, managerID, driver string, attributes map[string]any) error
	ClaimOutputHead(ctx context.Context, managerID string) (*sessionstore.OutputClaim, error)
	AckOutput(
		ctx context.Context,
		managerID string,
		outputID int64,
		attemptID string,
		messageIDs []string,
		sessionPatch map[string]any,
	) error
	RetryOutput(ctx context.Context, managerID string, outputID int64, attemptID, failure string, next time.Time) error
	BlockOutput(ctx context.Context, managerID string, outputID int64, attemptID, failure string) error
	RecoverInterruptedOutputs(ctx context.Context) (int64, error)
	RetryBlockedHead(ctx context.Context, managerID string) (bool, error)
	WakeOutputHead(ctx context.Context, managerID string) (bool, error)
	OutputQueueStatus(ctx context.Context, managerID string) (*sessionstore.OutputQueueStatus, error)
	LatestLifecycleOutputID(ctx context.Context, sessionID int64) (int64, error)
	ListUnresolvedOutputOwners(ctx context.Context) ([]string, error)
}
