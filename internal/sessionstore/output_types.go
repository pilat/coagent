package sessionstore

import (
	"errors"
	"time"
)

const (
	OutputMessage         OutputType = "message"
	OutputSessionOpened   OutputType = "session_opened"
	OutputSessionReplaced OutputType = "session_replaced"
	OutputSessionClosed   OutputType = "session_closed"
)

const managerIDAttribute = "manager_id"

// ModelInputGenerationAttribute is reserved for host-owned generation state;
// output producers cannot set it.
const ModelInputGenerationAttribute = "model_input_generation"

const killedReason = "killed"

const (
	outputAttributeName    = "name"
	outputAttributeWorkDir = "work_dir"
	outputSourceAgent      = "agent"
	outputSourceScheduler  = "scheduler"
)

var (
	ErrOutputConflict = errors.New("session output identity conflict")
	ErrOutputOwner    = errors.New("session output has no manager owner")
	ErrOutputNotRoot  = errors.New("session output belongs to a subagent")
)

type OutputType string

type OutputDraft struct {
	SessionID     int64
	Type          OutputType
	Content       string
	Attributes    map[string]any
	SourceKey     string
	ReleasesInput bool
	CreatedAt     time.Time
}

type OutputCommit struct {
	PersistOnly bool
	OutputID    int64
	OwnerID     string
	Existing    bool
	Content     string
	LiveContent string
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
