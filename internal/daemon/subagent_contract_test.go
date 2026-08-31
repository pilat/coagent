package daemon

import (
	"database/sql"

	"github.com/pilat/coagent/internal/subagent"
)

const (
	LinkStateSpawned   = subagent.StateSpawned
	LinkStateRunning   = subagent.StateRunning
	LinkStateCompleted = subagent.StateCompleted
	LinkStateError     = subagent.StateError
	LinkStateStopped   = subagent.StateStopped
	LinkStateKilled    = subagent.StateKilled

	LinkOutcomeCompleted  = subagent.OutcomeCompleted
	LinkOutcomeError      = subagent.OutcomeError
	LinkOutcomeKilled     = subagent.OutcomeKilled
	LinkOutcomeIncomplete = subagent.OutcomeIncomplete
)

type (
	LinkState    = subagent.State
	LinkOutcome  = subagent.Outcome
	SubagentLink = subagent.Link
	LinkStore    = subagent.Store
)

func NewLinkStore(db *sql.DB) LinkStore {
	return subagent.NewStore(db)
}
