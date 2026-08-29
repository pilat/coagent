package sessionstore

import (
	"context"
	"errors"
	"time"
)

type OutputType string

const (
	OutputMessageReplaceable OutputType = "message_replaceable"
	OutputMessagePersistent  OutputType = "message_persistent"
	OutputSessionOpened      OutputType = "session_opened"
	OutputSessionReplaced    OutputType = "session_replaced"
	OutputSessionClosed      OutputType = "session_closed"
)

type OutputState string

const (
	OutputStatePending    OutputState = "pending"
	OutputStateDelivering OutputState = "delivering"
	OutputStateRetryWait  OutputState = "retry_wait"
	OutputStateDelivered  OutputState = "delivered"
	OutputStateBlocked    OutputState = "blocked"
)

var (
	ErrNoOutput       = errors.New("manager has no deliverable output")
	ErrOutputConflict = errors.New("session output identity conflict")
	ErrOutputAttempt  = errors.New("session output attempt conflict")
	ErrManagerBinding = errors.New("manager binding conflict")
	ErrOutputOwner    = errors.New("session output has no manager owner")
	ErrOutputNotRoot  = errors.New("session output belongs to a subagent")
)

type OutputRetryPendingError struct{ NextAt time.Time }

func (e *OutputRetryPendingError) Error() string { return "manager output retry is not due" }

func (e *OutputRetryPendingError) Unwrap() error { return ErrNoOutput }

const managerIDAttribute = "manager_id"

// ModelInputGenerationAttribute is host-owned outbox metadata: producers cannot
// set it, and it never enters semantic output fingerprints.
const ModelInputGenerationAttribute = "model_input_generation"

const killedReason = "killed"

const (
	outputAttributeName    = "name"
	outputAttributeWorkDir = "work_dir"
	outputSourceAgent      = "agent"
	outputSourceScheduler  = "scheduler"
)

type OutputDraft struct {
	SessionID     int64
	Type          OutputType
	Content       string
	Attributes    map[string]any
	SourceKey     string
	Fingerprint   string
	ReleasesInput bool
	CreatedAt     time.Time
}

type OutputRecord struct {
	ID            int64
	SessionID     int64
	Type          OutputType
	Content       string
	Attributes    map[string]any
	SourceKey     string
	Fingerprint   string
	State         OutputState
	AttemptSeq    int64
	AttemptID     string
	LastAttemptAt *time.Time
	NextAttemptAt *time.Time
	DeliveredAt   *time.Time
	BlockedAt     *time.Time
	LastError     string
	CreatedAt     time.Time
	ReleasesInput bool
}

type OutputClaim struct {
	Output                  *OutputRecord
	SessionAttributes       map[string]any
	PreviousDeliveredOutput *OutputRecord
}

type OutputCommit struct {
	OutputID int64
	OwnerID  string
	Existing bool
}

type OutputQueueStatus struct {
	Pending       int
	BlockedID     int64
	BlockedAt     *time.Time
	DeliveryError string
}

// ManagerRootStore keeps manager-facing root lifecycle facts and their output
// obligations in the same transaction. The daemon uses it opportunistically so
// narrow store fakes do not acquire a second creation API.
type ManagerRootStore interface {
	CreateManagerRoot(ctx context.Context, create ManagerRootCreate) (*SessionRecord, *OutputCommit, error)
	ReplaceManagerRoot(
		ctx context.Context,
		oldSessionID int64,
		name, workDir string,
	) (*SessionRecord, *OutputCommit, error)
	ReplaceManagerRootForInput(
		ctx context.Context,
		oldSessionID, inputID int64,
		name, workDir string,
	) (*SessionRecord, *OutputCommit, error)
}

type ManagerRootCreate struct {
	ProjectID      int64
	Model          string
	ReasoningLevel string
	Attributes     map[string]any
	Prompt         string
	StartEpisode   bool
	Name           string
	WorkDir        string
}

type OutputStore interface {
	EnqueueOutput(ctx context.Context, draft OutputDraft) (*OutputCommit, error)
	InsertAssistantMessageWithOutput(
		ctx context.Context,
		sessionID int64,
		message *StoredMessage,
		outputType OutputType,
		content string,
	) (messageID int64, output *OutputCommit, err error)
	BindManager(ctx context.Context, managerID, driver string, attributes map[string]any) error
	ClaimOutputHead(ctx context.Context, managerID string) (*OutputClaim, error)
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
	OutputQueueStatus(ctx context.Context, managerID string) (*OutputQueueStatus, error)
}

type OutputIdentityStore interface { //nolint:iface // Optional reconciliation capability.
	OutputBySourceKey(ctx context.Context, sessionID int64, sourceKey string) (*OutputRecord, error)
}

// CommandOutputStore resolves an inbox command and its visible result together;
// normal input promotion remains owned by the session boundary.
type CommandOutputStore interface {
	HandleInputWithOutput(ctx context.Context, inputID int64, reason string, draft OutputDraft) (*OutputCommit, error)
}

// LifecycleOutputStore commits terminal state with its manager-visible output.
type LifecycleOutputStore interface {
	MarkSessionKilledWithOutput(ctx context.Context, sessionID int64) (*OutputCommit, error)
}

type LifecycleCommandStore interface {
	BeginLifecycleInput(ctx context.Context, inputID int64, command, content string) (*OutputCommit, error)
}

type ReplacementStore interface {
	ResolveReplacement(ctx context.Context, sessionID int64, managerID string) (int64, error)
}

// AssistantOutputStore commits an assistant transcript row and its manager
// output together. Session uses it opportunistically so in-memory stores remain
// usable in narrow unit tests.
//
//nolint:iface // optional runtime capability for persisted root sessions.
type AssistantOutputStore interface {
	InsertAssistantMessageWithOutput(
		ctx context.Context,
		sessionID int64,
		message *StoredMessage,
		outputType OutputType,
		content string,
	) (messageID int64, output *OutputCommit, err error)
}
