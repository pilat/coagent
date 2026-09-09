package builtin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/tool"
)

var _ tool.Tool = (*cancelProcessTool)(nil)

type cancelProcessParams struct {
	ProcessID string `json:"process_id"`
}

type cancelProcessTool struct {
	process   backgroundprocess.Service
	sessionID int64
}

func newCancelProcessTool(process backgroundprocess.Service, sessionID int64) tool.Tool {
	return &cancelProcessTool{process: process, sessionID: sessionID}
}

func (t *cancelProcessTool) ID() string         { return tool.IDCancelProcess }
func (t *cancelProcessTool) ParallelSafe() bool { return false }
func (t *cancelProcessTool) Description() string {
	return `Stop one background process started by this agent. Pass the opaque bgp_... ID shown in the background-start message, never an operating-system PID. Use this when the command is wrong, stuck, redundant, or no longer needed. Cancellation releases its process slot and produces no later completion. Do not cancel a useful process merely to check its status.`
}

func (t *cancelProcessTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"process_id": {
				"type": "string",
				"description": "Opaque bgp_... ID shown in the background-start message; not an operating-system PID"
			}
		},
		"required": ["process_id"]
	}`)
}

func (t *cancelProcessTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	var p cancelProcessParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid parameters: %w", err)
	}

	if p.ProcessID == "" {
		return nil, errors.New("process_id is required")
	}

	process, err := t.process.Store().GetProcess(ctx, p.ProcessID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("background process not found for this agent")
	}

	if err != nil {
		return nil, fmt.Errorf("load process: %w", err)
	}

	if process.SessionID != t.sessionID {
		return nil, errors.New("background process not found for this agent")
	}

	if process.State.Terminal() {
		return &tool.Result{
			Title:  "background process already finished",
			Output: fmt.Sprintf("Process %s is already %s; no cancellation was needed.", process.ID, process.State),
		}, nil
	}

	cancelled, err := t.process.CancelProcess(ctx, process.ID, backgroundprocess.IntentAgentCancelled)
	if err != nil {
		return nil, fmt.Errorf("cancel process: %w", err)
	}

	if cancelled == 0 {
		return &tool.Result{
			Title:  "background process already finished",
			Output: fmt.Sprintf("Process %s finished before cancellation; its process slot is available.", process.ID),
		}, nil
	}

	result := &tool.Result{
		Title: "background process cancelled",
		Output: fmt.Sprintf(
			"Cancelled process %s and released its process slot. No completion will arrive for it.",
			process.ID,
		),
	}

	return result, nil
}
