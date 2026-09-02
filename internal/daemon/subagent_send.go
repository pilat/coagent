package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/tool"
)

// SendToSubagentParams are the parameters for send_to_subagent.
type SendToSubagentParams struct {
	ID      int64  `json:"id"`
	Message string `json:"message"`
}

// sendToSubagentTool durably queues follow-up work for an existing
// subagent session, re-engaging it when necessary.
type sendToSubagentTool struct {
	spawner spawner
}

var _ tool.Tool = (*sendToSubagentTool)(nil)

// newSendToSubagentTool creates the send_to_subagent tool.
func newSendToSubagentTool(sp spawner) tool.Tool {
	return &sendToSubagentTool{spawner: sp}
}

func (t *sendToSubagentTool) ID() string { return tool.IDSendToSubagent }

func (t *sendToSubagentTool) ParallelSafe() bool { return false }

func (t *sendToSubagentTool) Description() string {
	return `Durably enqueue a follow-up message to the same subagent session previously launched with task, whether it was foreground or background.

Use this to add instructions or work while preserving that session's full context, including when re-engaging a finished subagent. A completed foreground subagent continues asynchronously because its original task call is already resolved. This is not a status check or a way to wait. End the current response or continue independent work; the next completion is delivered automatically and wakes the parent session. Do not use sleep, schedule, or polling.`
}

func (t *sendToSubagentTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"id": {
				"type": "integer",
				"description": "The subagent id returned by task"
			},
			"message": {
				"type": "string",
				"description": "The follow-up instruction for the subagent"
			}
		},
		"required": ["id", "message"]
	}`)
}

func (t *sendToSubagentTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	if t.spawner == nil {
		return nil, errors.New("subagents are not available in this context")
	}

	var p SendToSubagentParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid parameters: %w", err)
	}

	if p.ID == 0 {
		return nil, errors.New("id is required")
	}

	if p.Message == "" {
		return nil, errors.New("message is required")
	}

	if err := t.spawner.SendToChild(ctx, p.ID, p.Message); err != nil {
		return nil, fmt.Errorf("send to subagent: %w", err)
	}

	return &tool.Result{
		Title: fmt.Sprintf("sent to subagent #%d", p.ID),
		Output: fmt.Sprintf(
			"Follow-up durably accepted for subagent session #%d. Do not wait or poll in this turn; its next completion will be delivered automatically and wake this session.",
			p.ID,
		),
		Metadata: map[string]any{
			"id": p.ID,
		},
	}, nil
}
