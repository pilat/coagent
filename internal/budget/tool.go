//nolint:wrapcheck // Tool errors are already model-facing domain errors.; nosemgrep: semgrep.coagent-no-preamble-before-package
package budget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

const ToolID = "set_budget"

const noActivationMessage = "This change requires a current user message beginning with /budget."

type budgetTool struct {
	service Service
	rootID  int64
	priced  bool
}

type toolParams struct {
	Action   string   `json:"action"`
	CostUSD  *float64 `json:"cost_usd,omitempty"`
	Duration string   `json:"duration,omitempty"`
}

var (
	_ tool.Tool               = (*budgetTool)(nil)
	_ tool.ActivationDeclarer = (*budgetTool)(nil)
)

func NewTool(service Service, rootID int64, priced bool) tool.Tool {
	return &budgetTool{service: service, rootID: rootID, priced: priced}
}

func (t *budgetTool) ID() string { return ToolID }

func (t *budgetTool) ParallelSafe() bool { return false }

func (t *budgetTool) Description() string {
	return "Reads or changes the root session's one-shot cost/wall-time checkpoint. " +
		"action=get is read-only; set and clear require the current real user message to begin with /budget."
}

func (t *budgetTool) ActivationCommands() []string { return []string{"/budget"} }

func (t *budgetTool) Parameters() json.RawMessage {
	return json.RawMessage(
		`{"type":"object","properties":{"action":{"type":"string","enum":["get","set","clear"]},"cost_usd":{"type":"number"},"duration":{"type":"string"}},"required":["action"]}`,
	)
}

func (t *budgetTool) Execute(ctx context.Context, raw json.RawMessage) (*tool.Result, error) {
	var params toolParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, fmt.Errorf("invalid parameters: %w", err)
	}

	switch params.Action {
	case "get":
		if params.CostUSD != nil || params.Duration != "" {
			return nil, errors.New("get does not accept limit fields")
		}

		record, err := t.service.Get(ctx, t.rootID)
		if errors.Is(err, sessionstore.ErrBudgetNotFound) {
			return &tool.Result{Output: "No budget is configured."}, nil
		}

		if err != nil {
			return nil, err
		}

		return &tool.Result{Output: renderRecord(record)}, nil
	case "set":
		if !t.priced && params.CostUSD != nil {
			return nil, errors.New("cost budget unavailable: the current model has no catalog pricing")
		}

		cost, err := normalizeCost(params.CostUSD)
		if err != nil {
			return nil, err
		}

		duration, err := parseRelativeDuration(params.Duration)
		if err != nil {
			return nil, err
		}

		grant, err := currentGrant(ctx, t.rootID)
		if err != nil {
			return nil, err
		}

		record, receipt, err := t.service.Set(ctx, grant, cost, duration)
		if err != nil {
			return nil, err
		}

		return &tool.Result{Output: renderRecord(record), DirectMessages: []string{receipt}}, nil
	case "clear":
		if params.CostUSD != nil || params.Duration != "" {
			return nil, errors.New("clear does not accept limit fields")
		}

		grant, err := currentGrant(ctx, t.rootID)
		if err != nil {
			return nil, err
		}

		record, receipt, err := t.service.Clear(ctx, grant)
		if err != nil {
			return nil, err
		}

		return &tool.Result{Output: renderRecord(record), DirectMessages: []string{receipt}}, nil
	default:
		return nil, errors.New("action must be get, set, or clear")
	}
}

func currentGrant(ctx context.Context, rootID int64) (Grant, error) {
	value, ok := tool.ActivationGrantFromContext(ctx)

	callID := tool.CallIDFromContext(ctx)
	if !ok || value.SessionID != rootID || value.ToolID != ToolID || value.Command != "/budget" ||
		(value.ToolCallID != "" && value.ToolCallID != callID) {
		//nolint:staticcheck // Exact user-facing contract includes punctuation.
		return Grant{}, errors.New(noActivationMessage)
	}

	return Grant{
		RootID: rootID, InputID: value.InputID, ToolID: value.ToolID,
		Command: value.Command, ToolCallID: callID,
	}, nil
}

func renderRecord(record *sessionstore.BudgetRecord) string {
	if record == nil {
		return "No budget is configured."
	}

	return fmt.Sprintf("Budget %s (generation %d).", record.State, record.Generation)
}
