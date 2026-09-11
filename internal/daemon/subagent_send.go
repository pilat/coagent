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

Use this for related follow-up work on general or custom subagents while preserving that session's full context, including when re-engaging a finished subagent. Treat explore as a single research assignment; do not routinely resume it or request confirmation of its findings. A completed foreground subagent continues asynchronously because its original task call is already resolved. This is not a status check or a way to wait. Continue only useful independent work; the parent receives the next result automatically in a new turn. Do not use sleep, schedule, or polling. When this is your only remaining work, reply with a standalone <WAITING/> line and no tool calls.`
}

func (t *sendToSubagentTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"id": {
				"type": "integer",
				"description": "Numeric subagent_id shown in the task result; not a process ID or tool-call ID"
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
			"Follow-up durably accepted for subagent session #%d. Its next result will arrive automatically in a new turn; do not poll. "+
				"Do not poll with sleep, schedule, or get_subagent_result. "+
				"Do not poll with tools; continue only useful independent work. "+
				"When this is your only remaining work, reply with a standalone <WAITING/> line and no tool calls",
			p.ID,
		),
		Metadata: map[string]any{
			"id": p.ID,
		},
	}, nil
}
