package sessionstore

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/transcript"
)

const (
	CommitLoop CommitMode = iota
	CommitLifecycle
)

type CommitMode int

var ErrSessionStopping = errors.New("session is stopping")

// Commit applies one atomic session step; fired budgets suppress Unfired parts.
type Commit struct {
	SessionID, RootID int64
	At                time.Time
	Mode              CommitMode
	Accept            []Accept
	Messages          []*transcript.Message
	ToolResults       []*transcript.Message
	Replace           *Replace
	Activation        *ActivationChange
	ObserveBudget     bool
	Unfired           Parts
	State             StatePatch
	Outputs           []Output
}

type Accept struct {
	InputID                  int64
	State                    InputState
	Content, Reason, Receipt string
	LinkRef                  int
	ModelBound               bool
	InvalidateCompletion     bool
}

type Parts struct {
	Messages []*transcript.Message
	State    StatePatch
	Outputs  []Output
}

type Replace struct {
	HeadIDs []int64
	Entries []CompactionEntry
}
type ActivationChange struct {
	InputID                     int64
	State                       ActivationState
	ToolID, Command, ToolCallID string
}
type CandidateChange struct {
	Expected, Next int64
	NextRef        int
}

type StatePatch struct {
	Iteration            *int
	Status               *SessionStatus
	TodoItems            *json.RawMessage
	ContextBaseline      *ContextBaseline
	ClearContextBaseline bool
	Candidate            *CandidateChange
	ConfirmedAnswerID    *int64
	EmptyStopStreak      *int
	ManagerReplyPending  *bool
	ResetContext         bool
}

type Output struct {
	// PersistOnly outputs reach the manager through the outbox only, never as live events.
	PersistOnly   bool
	Type          OutputType
	Content       string
	Attributes    map[string]any
	Key           string
	MessageRef    int
	Phase         string
	ReleasesInput bool
	FinalFooter   *Footer
	Session       int64
}

type Footer struct{ BackgroundYield bool }

type CommitResult struct {
	MessageIDs  []int64
	Outputs     []*OutputCommit
	Activation  *ToolActivation
	Budget      *budget.Record
	BudgetFired bool
}
