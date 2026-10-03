package subagent

import (
	"context"
	"fmt"
)

// SpawnRequest describes a subagent to create. Built by the task tool at Execute
// time from its bound parent session id and the call id in context.
type SpawnRequest struct {
	ParentID       int64  // spawning session id
	AgentType      string // "general" | "explore" | custom subagent name
	AgentModel     string // agent type's model override; "" = none
	Prompt         string // initial task briefing
	Model          string // explicit task model param; "" = daemon resolves
	ReasoningLevel string // "" = inherit parent / default
	Blocking       bool   // true: parent suspends; false: background
	TaskCallID     string // spawning task tool_call id (from CallIDFromContext)
}

// ChildResult is a snapshot of a child's state, returned by Spawn/Result.
type ChildResult struct {
	ChildID   int64
	State     State
	Terminal  bool
	Output    string // final answer text / context note; "" when not terminal
	Iteration int
	Outcome   Outcome
}

// agentInfo describes an available subagent type for the task tool.
type agentInfo struct {
	Name        string
	Description string
}

// ModelInfo describes a model available for subagent selection.
type ModelInfo struct {
	ID   string
	Name string
	Tags []string
}

// Spawner starts and tracks children; Spawn returns immediately so the caller
// can release its run slot before waiting for a foreground result.
type Spawner interface {
	Spawn(ctx context.Context, req SpawnRequest) (ChildResult, error)
	Result(ctx context.Context, childID int64) (ChildResult, error)
	SendToChild(ctx context.Context, childID int64, msg string) error
	LinkPending(ctx context.Context, parentID int64, taskCallID string) (bool, error)
}

// FormatChildResult uses a neutral label for legacy links with no stored outcome.
func FormatChildResult(res ChildResult) string {
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
