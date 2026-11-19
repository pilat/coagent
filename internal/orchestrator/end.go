package orchestrator

import (
	"context"
	"fmt"

	"github.com/pilat/coagent/internal/tool"
)

const (
	IDStopSession = "stop_session"
	IDKillSession = "kill_session"

	stopDescription = "Stop a root session you started with spawn_session, as the human's /stop does: its running turn, sleep, background processes and subagents end; it keeps its conversation, and a later send_to_session wakes it. Refused for sessions you did not spawn (list_sessions marks yours), for this session itself, and for a session running under a budget the human set."
	killDescription = "Kill a root session you started with spawn_session, as the human's /kill does: everything it runs ends and it leaves /sessions for good; its directory and worktree stay on disk. Refused for sessions you did not spawn (list_sessions marks yours) and for this session itself."
	stopRunning     = "Stopped session #%d; its running turn was cancelled."
	stopQueued      = "Stopped session #%d; it had no running turn, and its queued and background work was cancelled."
	stopNothing     = "Session #%d is %s; nothing to stop."
	killChanged     = "Killed session #%d."
	killAlready     = "Session #%d was already killed."
)

// Ended describes the resolved target and whether its lifecycle command ran.
type Ended struct {
	ID      int64
	State   string
	Running bool
	Changed bool
}

func newEndTools(host Host, callerID int64) []tool.Tool {
	return []tool.Tool{
		&sessionTool{
			host:              host,
			callerID:          callerID,
			id:                IDStopSession,
			description:       stopDescription,
			parameters:        watchParameters,
			promptDescription: "stop a session you spawned",
			promptOrder:       7,
		},
		&sessionTool{
			host:              host,
			callerID:          callerID,
			id:                IDKillSession,
			description:       killDescription,
			parameters:        watchParameters,
			promptDescription: "kill a session you spawned",
			promptOrder:       8,
		},
	}
}

func (t *sessionTool) stop(ctx context.Context, targetID int64) (*tool.Result, error) {
	ended, err := t.host.StopSession(ctx, t.callerID, targetID)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	output := fmt.Sprintf(stopNothing, ended.ID, ended.State)
	if ended.Changed {
		output = fmt.Sprintf(stopQueued, ended.ID)
		if ended.Running {
			output = fmt.Sprintf(stopRunning, ended.ID)
		}
	}

	return &tool.Result{Title: fmt.Sprintf("stop #%d", ended.ID), Output: output}, nil
}

func (t *sessionTool) kill(ctx context.Context, targetID int64) (*tool.Result, error) {
	ended, err := t.host.KillSession(ctx, t.callerID, targetID)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	output := fmt.Sprintf(killAlready, ended.ID)
	if ended.Changed {
		output = fmt.Sprintf(killChanged, ended.ID)
	}

	return &tool.Result{Title: fmt.Sprintf("kill #%d", ended.ID), Output: output}, nil
}

func needsSessionID(id string) bool {
	switch id {
	case IDSendToSession, IDReadSession, IDSubscribe, IDUnsubscribe, IDStopSession, IDKillSession:
		return true
	default:
		return false
	}
}
