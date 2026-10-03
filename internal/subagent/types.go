package subagent

import (
	"context"
)

// State is the durable subagent-link lifecycle vocabulary.
type State string

const (
	StateSpawned   State = "spawned"
	StateRunning   State = "running"
	StateCompleted State = "completed"
	StateError     State = "error"
	StateStopped   State = "stopped"
	StateKilled    State = "killed"
)

// Outcome is the parent-facing completion vocabulary.
type Outcome string

const (
	OutcomeCompleted  Outcome = "completed"
	OutcomeError      Outcome = "error"
	OutcomeKilled     Outcome = "killed"
	OutcomeIncomplete Outcome = "incomplete"
)

// Link is a durable parent-child relationship and completion obligation.
type Link struct {
	ParentID         int64
	ChildID          int64
	TaskCallID       string
	Blocking         bool
	Depth            int
	State            State
	DeliveredAt      int64
	DeliveredMsgID   int64
	DeliveredInputID int64
	CreatedAt        int64
	ActivationSeq    int64
	Result           string
	Outcome          Outcome
}

// Create describes the child session, link, and initial input committed together.
type Create struct {
	ProjectID      int64
	ParentID       int64
	RootID         int64
	AgentType      string
	Model          string
	ReasoningLevel string
	TaskCallID     string
	Blocking       bool
	Depth          int
	State          State
	InitialInput   string
}

// Store owns subagent links and atomic cross-table lifecycle transitions.
type Store interface {
	interface {
		GetLink(ctx context.Context, childID int64) (*Link, error)
		GetLinkByTaskCallID(ctx context.Context, parentID int64, taskCallID string) (*Link, error)
		ListPendingChildLinks(ctx context.Context, parentID int64) ([]Link, error)
		ListRunningChildLinks(ctx context.Context) ([]Link, error)
		ListUndeliveredParentLinks(ctx context.Context) ([]Link, error)
	}
	MarkLinkStopped(ctx context.Context, childID int64) error
	MakeStoppedLinkResumable(ctx context.Context, childID int64) error
	Create(ctx context.Context, create Create) (int64, error)
	Finalize(ctx context.Context, childID int64, errored bool) (*Link, error)
	Kill(ctx context.Context, childID int64) error
	Resume(ctx context.Context, childID int64) error
	Rearm(ctx context.Context, childID int64) (bool, error)
	DeliverCompletion(ctx context.Context, link Link, content string) (won bool, err error)
	DeliverBackgroundCompletion(ctx context.Context, link Link, iterations int) (won bool, err error)
}

// Terminal reports whether the current activation has finished.
func (l Link) Terminal() bool {
	switch l.State {
	case StateCompleted, StateError, StateKilled:
		return true
	case StateSpawned, StateRunning, StateStopped:
		return false
	default:
		return false
	}
}

func (s State) valid() bool {
	switch s {
	case StateSpawned, StateRunning, StateCompleted, StateError, StateStopped, StateKilled:
		return true
	default:
		return false
	}
}

func (o Outcome) valid() bool {
	switch o {
	case OutcomeCompleted, OutcomeError, OutcomeKilled, OutcomeIncomplete:
		return true
	default:
		return false
	}
}

func validTerminalLink(state State, outcome Outcome) bool {
	switch state {
	case StateCompleted:
		return outcome == OutcomeCompleted || outcome == OutcomeIncomplete
	case StateError:
		return outcome == OutcomeError || outcome == OutcomeIncomplete
	case StateKilled:
		return outcome == OutcomeKilled
	case StateSpawned, StateRunning, StateStopped:
		return false
	default:
		return false
	}
}
