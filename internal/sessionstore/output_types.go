package sessionstore

import (
	"errors"
	"time"
)

const (
	OutputMessageReplaceable OutputType = "message_replaceable"
	OutputMessagePersistent  OutputType = "message_persistent"
	OutputSessionOpened      OutputType = "session_opened"
	OutputSessionReplaced    OutputType = "session_replaced"
	OutputSessionClosed      OutputType = "session_closed"
)

const (
	OutputStatePending    OutputState = "pending"
	OutputStateDelivering OutputState = "delivering"
	OutputStateRetryWait  OutputState = "retry_wait"
	OutputStateDelivered  OutputState = "delivered"
	OutputStateBlocked    OutputState = "blocked"
)

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

var (
	ErrNoOutput       = errors.New("manager has no deliverable output")
	ErrOutputConflict = errors.New("session output identity conflict")
	ErrOutputAttempt  = errors.New("session output attempt conflict")
	ErrManagerBinding = errors.New("manager binding conflict")
	ErrOutputOwner    = errors.New("session output has no manager owner")
	ErrOutputNotRoot  = errors.New("session output belongs to a subagent")
)

type OutputType string

type OutputState string

type OutputRetryPendingError struct{ NextAt time.Time }

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
	Content  string
}

type OutputQueueStatus struct {
	Pending       int
	BlockedID     int64
	BlockedAt     *time.Time
	DeliveryError string
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

func (e *OutputRetryPendingError) Error() string { return "manager output retry is not due" }

func (e *OutputRetryPendingError) Unwrap() error { return ErrNoOutput }
