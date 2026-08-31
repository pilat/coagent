package sessionstore

import "context"

// AgentRuntimeStore owns transcript/checkpoint persistence and the budgeted
// response transactions used by one live agent loop.
type AgentRuntimeStore interface {
	RuntimeStore
	BudgetResponseStore
	BudgetCompactionStore
}

// ManagerOutputStore owns the durable delivery ledger and its manager-facing
// recovery projections.
type ManagerOutputStore interface {
	OutputStore
	LifecycleOutputHistoryStore
	OutputOwnerStore
}

// ManagerRootTransactions owns atomic root creation, replacement, and legacy
// ownership claims.
type ManagerRootTransactions interface {
	ManagerRootStore
	ReplacementStore
	LegacyCLIClaimStore
}

// SessionLifecycleStore owns command settlement and terminal output
// transactions that must commit with session lifecycle state.
type SessionLifecycleStore interface {
	CommandOutputStore
	LifecycleCommandStore
	LifecycleOutputStore
	StopCompletionStore
	CancelPendingInputs(context.Context, []int64, string) (int64, error)
}

var (
	_ AgentRuntimeStore       = (*store)(nil)
	_ ManagerOutputStore      = (*store)(nil)
	_ ManagerRootTransactions = (*store)(nil)
	_ SessionLifecycleStore   = (*store)(nil)
)
