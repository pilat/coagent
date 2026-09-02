package daemon

import (
	"context"
	"fmt"

	"github.com/pilat/coagent/internal/subagent"
)

// spawnRequest describes a subagent to create. Built by the task tool at Execute
// time from its bound parent session id and the call id in context.
type spawnRequest struct {
	ParentID       int64  // spawning session id
	AgentType      string // "general" | "explore" | custom subagent name
	AgentModel     string // agent type's model override; "" = none
	Prompt         string // initial task briefing
	Model          string // explicit task model param; "" = daemon resolves
	ReasoningLevel string // "" = inherit parent / default
	Blocking       bool   // true: parent suspends; false: background
	TaskCallID     string // spawning task tool_call id (from CallIDFromContext)
}

// childResult is a snapshot of a child's state, returned by Spawn/Result.
type childResult struct {
	ChildID   int64
	State     subagent.State
	Terminal  bool
	Output    string // final answer text / context note; "" when not terminal
	Iteration int
	Outcome   subagent.Outcome
}

// subagentInfo describes an available subagent type for the task tool.
type subagentInfo struct {
	Name        string
	Description string
}

// modelInfo describes a model available for subagent selection.
type modelInfo struct {
	ID   string
	Name string
	Tags []string
}

// spawner creates and tracks subagent sessions. The daemon svc implements it;
// the task / get_subagent_result / send_to_subagent tools consume it.
//
// Spawn returns an instant child id; it never waits and never returns ErrSuspend.
// Business errors (caps, unknown type) come back as error and are
// rendered into a tool_result. Suspension is the parent's decision after a
// successful blocking spawn.
type spawner interface {
	Spawn(ctx context.Context, req spawnRequest) (childResult, error)
	Result(ctx context.Context, childID int64) (childResult, error)
	SendToChild(ctx context.Context, childID int64, msg string) error
	LinkPending(ctx context.Context, parentID int64, taskCallID string) (bool, error)
}

var _ spawner = (*svc)(nil)

// formatChildResult renders a terminal child's outcome + result text for the
// parent. The daemon's auto-delivered completion and get_subagent_result both
// format through this so they surface an identical string. A blank Outcome
// (a link terminalized by an older binary and redelivered post-upgrade) falls
// back to a neutral "finished" label rather than emitting malformed content.
func formatChildResult(res childResult) string {
	outcome := string(res.Outcome)
	if outcome == "" {
		outcome = "finished"
	}

	body := res.Output
	if body == "" {
		body = "(no output)"
	}

	return fmt.Sprintf("Subagent #%d %s (%d iterations).\n\n%s", res.ChildID, outcome, res.Iteration, body)
}
