package subagent

import (
	"context"

	"github.com/pilat/coagent/internal/transcript"
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
	ParentID       int64
	ChildID        int64
	TaskCallID     string
	Blocking       bool
	Depth          int
	State          State
	DeliveredAt    int64
	DeliveredMsgID int64
	TimeoutSec     int
	CreatedAt      int64
	ActivationSeq  int64
	Result         string
	Outcome        Outcome
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
	TimeoutSec     int
	InitialInput   string
}

// Store owns ordinary durable subagent-ledger access.
type Store interface {
	InsertSubagentLink(ctx context.Context, link Link) error
	GetLink(ctx context.Context, childID int64) (*Link, error)
	GetLinkByTaskCallID(ctx context.Context, parentID int64, taskCallID string) (*Link, error)
	ListPendingChildLinks(ctx context.Context, parentID int64) ([]Link, error)
	ListRunningChildLinks(ctx context.Context) ([]Link, error)
	ListUndeliveredParentLinks(ctx context.Context) ([]Link, error)
	MarkLinkTerminal(ctx context.Context, childID int64, state State, result string, outcome Outcome) error
	ResetLinkRunning(ctx context.Context, childID int64) error
	MarkLinkStopped(ctx context.Context, childID int64) error
	MakeStoppedLinkResumable(ctx context.Context, childID int64) error
}

// Transactions owns cross-table transitions that preserve subagent obligations.
type Transactions interface {
	Create(ctx context.Context, create Create) (int64, error)
	TryFinalizeActivation(
		ctx context.Context,
		childID int64,
		state State,
		result string,
		outcome Outcome,
	) (bool, error)
	DeliverCompletion(
		ctx context.Context,
		parentID int64,
		messages []*transcript.Message,
		childID int64,
		activationSeq int64,
	) (messageIDs []int64, won bool, err error)
	RearmDeliveredWithPendingInput(ctx context.Context, childID int64) (bool, error)
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
