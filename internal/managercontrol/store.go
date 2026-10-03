package managercontrol

import (
	"context"
	"time"

	"github.com/pilat/coagent/internal/sessionstore"
)

// Store is the persistence view required by managercontrol.
type Store interface {
	GetSession(context.Context, int64) (*sessionstore.SessionRecord, error)
	ListSessions(context.Context) ([]*sessionstore.SessionRecord, error)
	GetOrCreateProject(context.Context, string) (int64, error)
	GetOrCreateNamedProject(context.Context, string, string) (int64, error)
	GetOrCreateHiddenProject(context.Context, string) (int64, error)
	GetProjectWorkDir(context.Context, int64) (string, error)
	GetProjectName(context.Context, int64) (string, error)
	ListProjects(context.Context) ([]sessionstore.ProjectRow, error)
	LatestActivityByProject(context.Context, []int64) (map[int64]time.Time, error)
	EnsureManagementRoot(
		context.Context,
		int64,
		string,
		int64,
		string,
		string,
	) (*sessionstore.SessionRecord, *sessionstore.OutputCommit, error)
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
