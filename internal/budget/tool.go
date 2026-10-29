//nolint:wrapcheck // Tool errors are already model-facing domain errors.; nosemgrep: semgrep.coagent-no-preamble-before-package
package budget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/pilat/coagent/internal/tool"
)

const ToolID = "set_budget"

const noActivationMessage = "This change requires a current user message beginning with /budget."

var (
	_ tool.Tool               = (*budgetTool)(nil)
	_ tool.ActivationDeclarer = (*budgetTool)(nil)
)

type budgetTool struct {
	store   Store
	rootID  int64
	priced  bool
	resolve func(context.Context, int64) (int64, bool, error)
}

type toolParams struct {
	Action   string          `json:"action"`
	CostUSD  *float64        `json:"cost_usd,omitempty"`
	Duration string          `json:"duration,omitempty"`
	Session  json.RawMessage `json:"session,omitempty"`
}

func NewTool(store Store, rootID int64, priced bool) tool.Tool {
	return &budgetTool{store: store, rootID: rootID, priced: priced}
}

// NewTargetedTool keeps grants on the caller while resolving current target authority and pricing.
func NewTargetedTool(store Store, rootID int64, resolve func(context.Context, int64) (int64, bool, error)) tool.Tool {
	return &budgetTool{store: store, rootID: rootID, resolve: resolve}
}

func (t *budgetTool) ID() string { return ToolID }

func (t *budgetTool) ParallelSafe() bool { return false }

func (t *budgetTool) Description() string {
	if t.resolve != nil {
		return "Reads or changes a work session's one-shot cost/wall-time checkpoint; omit session for your own root. " +
			"action=get is read-only; set and clear require the current real user message to begin with /budget."
	}

	return "Reads or changes the root session's one-shot cost/wall-time checkpoint. " +
		"action=get is read-only; set and clear require the current real user message to begin with /budget."
}

func (t *budgetTool) ActivationCommands() []string { return []string{"/budget"} }

func (t *budgetTool) Parameters() json.RawMessage {
	if t.resolve != nil {
		return json.RawMessage(
			`{"type":"object","properties":{"action":{"type":"string","enum":["get","set","clear"]},"cost_usd":{"type":"number"},"duration":{"type":"string"},"session":{"type":"string","description":"A session id like \"244\" or \"#244\"; omit for your own budget."}},"required":["action"]}`,
		)
	}

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
		return t.get(ctx, params)
	case "set":
		return t.set(ctx, params)
	case "clear":
		return t.clear(ctx, params)
	default:
		return nil, errors.New("action must be get, set, or clear")
	}
}

func (t *budgetTool) get(ctx context.Context, params toolParams) (*tool.Result, error) {
	if params.CostUSD != nil || params.Duration != "" {
		return nil, errors.New("get does not accept limit fields")
	}

	root, _, err := t.target(ctx, params.Session)
	if err != nil {
		return nil, err
	}

	record, err := t.store.Get(ctx, root)
	if errors.Is(err, ErrNotFound) {
		return &tool.Result{Output: "No budget is configured."}, nil
	}

	if err != nil {
		return nil, err
	}

	return &tool.Result{Output: renderRecord(record)}, nil
}

func (t *budgetTool) set(ctx context.Context, params toolParams) (*tool.Result, error) {
	root, priced, err := t.target(ctx, params.Session)
	if err != nil {
		return nil, err
	}

	if !priced && params.CostUSD != nil {
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

	if cost == nil && duration == nil {
		return nil, errors.New("set requires cost_usd, duration, or both")
	}
	var seconds *int64

	if duration != nil {
		value := int64(duration.Seconds())
		seconds = &value
	}

	receipt := "Budget armed" + t.targetSuffix(root) + ": " + renderLimits(cost, duration)

	record, err := t.store.Arm(ctx, Mutation{
		RootSessionID: root, GrantSessionID: grant.RootID, InputID: grant.InputID, ToolID: grant.ToolID,
		Command: grant.Command, ToolCallID: grant.ToolCallID,
		CostLimitUSD: cost, DurationSeconds: seconds, Receipt: receipt,
	})
	if err != nil {
		return nil, err
	}

	return &tool.Result{Output: renderRecord(record), DirectMessages: []string{receipt}}, nil
}

func (t *budgetTool) clear(ctx context.Context, params toolParams) (*tool.Result, error) {
	if params.CostUSD != nil || params.Duration != "" {
		return nil, errors.New("clear does not accept limit fields")
	}

	grant, err := currentGrant(ctx, t.rootID)
	if err != nil {
		return nil, err
	}

	root, _, err := t.target(ctx, params.Session)
	if err != nil {
		return nil, err
	}

	receipt := "Budget cleared" + t.targetSuffix(root)

	record, err := t.store.Clear(ctx, Mutation{
		RootSessionID: root, GrantSessionID: grant.RootID, InputID: grant.InputID, ToolID: grant.ToolID,
		Command: grant.Command, ToolCallID: grant.ToolCallID, Receipt: receipt,
	})
	if err != nil {
		return nil, err
	}

	return &tool.Result{Output: renderRecord(record), DirectMessages: []string{receipt}}, nil
}

func (t *budgetTool) target(ctx context.Context, raw json.RawMessage) (int64, bool, error) {
	if t.resolve == nil {
		return t.rootID, t.priced, nil
	}

	var id int64

	if len(raw) != 0 {
		var text string
		if err := json.Unmarshal(raw, &text); err == nil {
			parsed, parseErr := strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(text), "#"), 10, 64)
			if parseErr != nil {
				return 0, false, errors.New("session must be a session id")
			}

			id = parsed
		} else if err := json.Unmarshal(raw, &id); err != nil {
			return 0, false, errors.New("session must be a session id")
		}
	}

	if id < 0 {
		return 0, false, errors.New("session must be a session id")
	}

	return t.resolve(ctx, id)
}

func (t *budgetTool) targetSuffix(root int64) string {
	if root == t.rootID {
		return ""
	}

	return fmt.Sprintf(" for #%d", root)
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

func renderRecord(record *Record) string {
	if record == nil {
		return "No budget is configured."
	}

	return fmt.Sprintf("Budget %s (generation %d).", record.State, record.Generation)
}
