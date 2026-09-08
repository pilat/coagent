package backgroundprocess

import "time"

// State is the durable process lifecycle vocabulary.
type State string

const (
	StateRunning             State = "running"
	StateCompleted           State = "completed"
	StateFailed              State = "failed"
	StateTimedOut            State = "timed_out"
	StateOutputLimitExceeded State = "output_limit_exceeded"
	StateOutputDrainTimeout  State = "output_drain_timeout"
	StateCancelled           State = "cancelled"
	StateInterrupted         State = "interrupted"
)

// HostIntent is a terminal cause recorded before Wait observes an outcome.
// Natural exit wins only when no intent was recorded.
type HostIntent string

const (
	IntentNone           HostIntent = ""
	IntentDeadline       HostIntent = "deadline"
	IntentOutputLimit    HostIntent = "output_limit_exceeded"
	IntentSessionStopped HostIntent = "session_stopped"
	IntentSessionKilled  HostIntent = "session_killed"
	IntentDaemonShutdown HostIntent = "daemon_shutdown"
)

// IntentToState maps a recorded host intent to the terminal state it decides.
func IntentToState(intent HostIntent) State {
	switch intent {
	case IntentDeadline:
		return StateTimedOut
	case IntentOutputLimit:
		return StateOutputLimitExceeded
	case IntentSessionStopped, IntentSessionKilled:
		return StateCancelled
	case IntentDaemonShutdown:
		return StateInterrupted
	case IntentNone:
		return StateFailed
	default:
		return StateFailed
	}
}

// Process is the durable background-process ledger row.
type Process struct {
	ID                      string
	SessionID               int64
	RootSessionID           int64
	ToolCallID              string
	OutputPath              string
	Deadline                time.Time
	CreatedAt               time.Time
	AdvertisedAt            *time.Time
	OutputSize              int64
	ExitCode                *int
	HostIntent              HostIntent
	State                   State
	FinishedAt              *time.Time
	DeliveryState           string
	DeliveryTargetSessionID int64
	DeliveredAt             *time.Time
}

// Terminal reports whether the process reached a terminal state.
func (s State) Terminal() bool {
	return s != StateRunning
}

// WakeSuppressed reports whether stop/kill suppressed this process's wake event.
func (p Process) WakeSuppressed() bool {
	return p.HostIntent == IntentSessionStopped || p.HostIntent == IntentSessionKilled
}
