package orchestrator

import "fmt"

const (
	SignalAnswered SignalKind = "answered"
	SignalError    SignalKind = "error"
	SignalStopped  SignalKind = "stopped"
	SignalParked   SignalKind = "parked"
	SignalKilled   SignalKind = "killed"
	SignalCleared  SignalKind = "cleared"
)

// SignalKind identifies a root's turn outcome without carrying its output.
type SignalKind string

// Actor names the delegated author of a lifecycle command; zero means the human.
type Actor struct {
	Session int64
	App     string
}

// SignalText keeps the display name in chat and out of the model line.
func SignalText(k SignalKind, id int64, project, name string, replacement int64, actor Actor) (string, string) {
	var fact, notice string

	readID := id

	switch k {
	case SignalAnswered:
		fact, notice = "finished a turn with an answer", "answered"
	case SignalError:
		fact, notice = "stopped on an error", "stopped on an error"
	case SignalStopped:
		fact, notice = "was stopped by the human with /stop", "was stopped"
		if actor.Session != 0 {
			fact = fmt.Sprintf("was stopped by session #%d with %s", actor.Session, IDStopSession)
			notice = fmt.Sprintf("was stopped by #%d", actor.Session)
		} else if actor.App != "" {
			notice = "was stopped by app " + actor.App
			fact = notice + " with StopSession"
		}
	case SignalParked:
		fact = "is parked at its budget checkpoint; only the human can resume it"
		notice = "is parked at its budget checkpoint"
	case SignalKilled:
		fact, notice = "was killed", "was killed"
		if actor.Session != 0 {
			fact = fmt.Sprintf("was killed by session #%d with %s", actor.Session, IDKillSession)
			notice = fmt.Sprintf("was killed by #%d", actor.Session)
		} else if actor.App != "" {
			notice = "was killed by app " + actor.App
			fact = notice + " with KillSession"
		}
	case SignalCleared:
		fact = fmt.Sprintf("was cleared by the human; its replacement is session #%d", replacement)
		notice = fmt.Sprintf("was cleared; now #%d", replacement)
		readID = replacement
	}

	line := fmt.Sprintf(
		"[Signal from coagent: session #%d in project %s %s. Read it with read_session (session_id %d); no further signal comes for #%d until you do.]",
		id,
		project,
		fact,
		readID,
		readID,
	)
	chat := fmt.Sprintf("🔔 #%d %s %s", id, name, notice)

	return line, chat
}
