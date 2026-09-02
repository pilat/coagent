package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/tool"
)

// getSubagentResultParams are the parameters for get_subagent_result.
type getSubagentResultParams struct {
	ID int64 `json:"id"`
}

// getSubagentResultTool reads a diagnostic snapshot of a subagent.
type getSubagentResultTool struct {
	spawner spawner
}

var _ tool.Tool = (*getSubagentResultTool)(nil)

// newGetSubagentResultTool creates the get_subagent_result tool.
func newGetSubagentResultTool(sp spawner) tool.Tool {
	return &getSubagentResultTool{spawner: sp}
}

func (t *getSubagentResultTool) ID() string { return "get_subagent_result" }

func (t *getSubagentResultTool) ParallelSafe() bool { return false }

func (t *getSubagentResultTool) Description() string {
	return `Read a one-off diagnostic snapshot of a subagent previously launched with task.

Returns the subagent's current state (running, completed, error, killed) and, once terminal, its final output. This is for inspection and troubleshooting, not waiting: do not poll this tool and do not call sleep or schedule for a subagent. Completion is delivered automatically as a subagent_event and wakes the parent session.`
}

func (t *getSubagentResultTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"id": {
				"type": "integer",
				"description": "The subagent id returned by task"
			}
		},
		"required": ["id"]
	}`)
}

func (t *getSubagentResultTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	if t.spawner == nil {
		return nil, errors.New("subagents are not available in this context")
	}

	var p getSubagentResultParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid parameters: %w", err)
	}

	if p.ID == 0 {
		return nil, errors.New("id is required")
	}

	res, err := t.spawner.Result(ctx, p.ID)
	if err != nil {
		return nil, fmt.Errorf("get subagent result: %w", err)
	}

	var output string
	if res.Terminal {
		output = formatChildResult(res)
	} else {
		output = fmt.Sprintf(
			"Subagent #%d is %s (%d iterations). No result yet. This is a diagnostic snapshot; its completion will be delivered automatically and wake this session.",
			res.ChildID,
			res.State,
			res.Iteration,
		)
	}

	return &tool.Result{
		Title:  fmt.Sprintf("subagent #%d: %s", res.ChildID, res.State),
		Output: output,
		Metadata: map[string]any{
			"id":        res.ChildID,
			"state":     res.State,
			"outcome":   res.Outcome,
			"terminal":  res.Terminal,
			"iteration": res.Iteration,
		},
	}, nil
}
