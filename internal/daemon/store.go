package daemon

import (
	"context"
	"time"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/transcript"
)

// Store is the persistence view required by daemon.
type Store interface { //nolint:interfacebloat // Lifecycle commands share atomic session, inbox and output state.
	Commit(context.Context, sessionstore.Commit) (*sessionstore.CommitResult, error)
	ListPending(context.Context, int64) ([]*sessionstore.InboxInput, error)
	PendingActivation(context.Context, int64) (*sessionstore.ToolActivation, error)
	ObserveBudget(context.Context, int64, time.Time, string) (*budget.Record, bool, error)
	LoadActiveMessages(context.Context, int64) ([]*transcript.Message, error)
	CreateSession(
		ctx context.Context,
		projectID int64,
		model, reasoningLevel string,
		attrs map[string]any,
	) (*sessionstore.SessionRecord, error)
	CreateReplacementSession(ctx context.Context, oldSessionID int64) (*sessionstore.SessionRecord, error)
	SetAttributes(ctx context.Context, id int64, attrs map[string]any) error
	UpdateSessionModel(ctx context.Context, id int64, model, reasoningLevel string) error
	GetSession(ctx context.Context, id int64) (*sessionstore.SessionRecord, error)
	ListSessions(ctx context.Context) ([]*sessionstore.SessionRecord, error)
	ListAllSessions(ctx context.Context) ([]*sessionstore.SessionRecord, error)
	LatestActivityByProject(ctx context.Context, projectIDs []int64) (map[int64]time.Time, error)
	MarkSessionKilled(ctx context.Context, id int64) error
	UpdateSessionStatus(ctx context.Context, id int64, status sessionstore.SessionStatus) error
	LoadMessageContentByID(ctx context.Context, sessionID, messageID int64) (string, error)
	LoadCurrentTerminalRejection(ctx context.Context, sessionID int64) (*transcript.Message, error)
	Enqueue(context.Context, sessionstore.Input) (*sessionstore.Enqueued, error)
	Woken() <-chan struct{}
	TakeWoken() []int64
	PeekPending(context.Context, int64) (*sessionstore.InboxInput, error)
	CancelPendingInputsForStop(context.Context, []int64, string) (int64, error)
	HasAcceptedInput(context.Context, int64) (bool, error)
	ListSessionsWithRecoverableInput(context.Context) ([]int64, error)
	EnqueueOutput(ctx context.Context, draft sessionstore.OutputDraft) (*sessionstore.OutputCommit, error)
	WakeOutputHead(ctx context.Context, managerID string) (bool, error)
	CreateManagerRoot(ctx context.Context, create sessionstore.ManagerRootCreate) (*sessionstore.SessionRecord, *sessionstore.OutputCommit, error)
	EnsureManagementRoot(
		ctx context.Context,
		projectID int64,
		owner string,
		topicID int64,
		name, workDir string,
	) (*sessionstore.SessionRecord, *sessionstore.OutputCommit, error)
	ReplaceManagerRoot(
		ctx context.Context,
		oldSessionID int64,
		name, workDir string,
	) (*sessionstore.SessionRecord, *sessionstore.OutputCommit, error)
	ReplaceManagerRootForInput(
		ctx context.Context,
		oldSessionID, inputID int64,
		name, workDir string,
	) (*sessionstore.SessionRecord, *sessionstore.OutputCommit, error)
	ResolveReplacement(ctx context.Context, sessionID int64, managerID string) (int64, error)
	RecordSessionStartFailure(context.Context, int64, string) (bool, error)
	BeginLifecycleInput(ctx context.Context, inputID int64, command, content string) (*sessionstore.OutputCommit, error)
	MarkSessionKilledWithOutput(
		ctx context.Context,
		sessionID int64,
		cancelledProcesses int,
	) (*sessionstore.OutputCommit, error)
	CompleteExplicitStop(
		ctx context.Context,
		rootID, inputID int64,
		cancelledProcesses int,
	) (*sessionstore.OutputCommit, error)
	SelectInterruptedExplicitStops(ctx context.Context) ([]sessionstore.InterruptedExplicitStop, error)
	Get(ctx context.Context, rootID int64) (*budget.Record, error)
	Arm(ctx context.Context, mutation budget.Mutation) (*budget.Record, error)
	Clear(ctx context.Context, mutation budget.Mutation) (*budget.Record, error)
	HasBackgroundObligationByRoot(ctx context.Context, rootID int64) (bool, error)
	GetOrCreateProject(ctx context.Context, workDir string) (int64, error)
	GetOrCreateNamedProject(ctx context.Context, workDir, name string) (int64, error)
	GetOrCreateHiddenProject(ctx context.Context, workDir string) (int64, error)
	GetProjectWorkDir(ctx context.Context, projectID int64) (string, error)
	GetProjectName(ctx context.Context, projectID int64) (string, error)
	ListProjects(ctx context.Context) ([]sessionstore.ProjectRow, error)
	ReactivateForSchedule(context.Context, int64) (bool, error)
}
