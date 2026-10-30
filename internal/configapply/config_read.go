package configapply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/pilat/coagent/internal/tool"
)

var _ tool.Tool = (*configReadTool)(nil)

type configReadTool struct {
	service   *Service
	sessionID int64
}

// NewConfigRead shares config_edit's session grant without declaring another /config route.
func NewConfigRead(sessionID int64, service *Service) tool.Tool {
	return &configReadTool{service: service, sessionID: sessionID}
}

func (t *configReadTool) ID() string { return tool.IDConfigRead }

func (t *configReadTool) ParallelSafe() bool { return true }

func (t *configReadTool) Description() string {
	return "Read the current complete configuration document verbatim before config_edit. " +
		"Requires the current user's /config turn, sharing config_edit's authority."
}

func (t *configReadTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
}

func (t *configReadTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	var empty struct{}
	if err := json.Unmarshal(params, &empty); err != nil {
		return nil, fmt.Errorf("parse config read parameters: %w", err)
	}

	if err := requireActivation(ctx); err != nil {
		return nil, err
	}

	grant, _ := tool.ActivationGrantFromContext(ctx)
	if grant.SessionID != t.sessionID {
		//nolint:staticcheck // The shared activation refusal is the authority contract.
		return nil, errors.New(noConfigActivationMessage)
	}

	if t.service == nil || t.service.Ops() == nil {
		return nil, errors.New("this daemon cannot read its configuration")
	}

	document, err := os.ReadFile(t.service.Ops().ConfigPath())
	if err != nil {
		return nil, fmt.Errorf("read current configuration: %w", err)
	}

	return &tool.Result{Output: string(document)}, nil
}
