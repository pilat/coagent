package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/tool"
)

// TaskParams are the parameters for the task tool.
type TaskParams struct {
	Prompt       string `json:"prompt"`
	Description  string `json:"description"`
	SubagentType string `json:"subagent_type"`
	Model        string `json:"model,omitempty"`      // model for the subagent
	Timeout      int    `json:"timeout,omitempty"`    // seconds
	Background   bool   `json:"background,omitempty"` // true: spawn and return immediately
}

// taskTool spawns subagents to handle complex tasks.
type taskTool struct {
	spawner       spawner
	parentID      int64
	set           *registry.Set
	subagentTypes []subagentInfo
	modelCatalog  []modelInfo
}

var _ tool.Tool = (*taskTool)(nil)

// newTaskTool creates the task tool bound to a spawning session. spawner drives
// subagents via the daemon (blocking suspends the parent, background returns
// immediately); parentID is the spawning session's id. Available subagent types
// come from the session's agent-type set (built-ins plus project-local ones).
func newTaskTool(
	sp spawner,
	parentID int64,
	set *registry.Set,
	modelCatalog []modelInfo,
) tool.Tool {
	subagents := set.ListSubagents()

	subagentTypes := make([]subagentInfo, 0, len(subagents))
	for _, cfg := range subagents {
		subagentTypes = append(subagentTypes, subagentInfo{Name: string(cfg.Name), Description: cfg.Description})
	}

	candidates := make([]modelInfo, 0, len(modelCatalog))
	for _, model := range modelCatalog {
		if len(model.Tags) != 0 {
			candidates = append(candidates, model)
		}
	}

	return &taskTool{
		spawner:       sp,
		parentID:      parentID,
		set:           set,
		subagentTypes: subagentTypes,
		modelCatalog:  candidates,
	}
}

func (t *taskTool) ID() string { return tool.IDTask }

func (t *taskTool) Description() string {
	types := t.subagentTypes
	models := t.modelCatalog

	var typeList strings.Builder
	for _, info := range types {
		fmt.Fprintf(&typeList, "- %s: %s\n", info.Name, info.Description)
	}

	var b strings.Builder
	b.WriteString("\nAvailable models for subagents:\n- inherit (default): use the current session model\n")

	for _, m := range models {
		fmt.Fprintf(&b, "- %s: %s (tags: %s)\n", m.ID, m.Name, strings.Join(m.Tags, ", "))
	}

	modelList := b.String()

	return fmt.Sprintf(`Launch a subagent to work autonomously with its own context and tools.

Available subagent types:
%s
When to use task:
- Work requiring more than 3 tool calls — delegate instead of grinding
- 2+ independent work items — launch in parallel (multiple task calls in one response)
- Research before implementation — explore first, then general

When NOT to use task:
- 3 or fewer total tool calls — just do it directly
- Single file read, search, or edit — use the dedicated tool

ALWAYS launch independent tasks in parallel. Multiple task tool calls in one response, not sequential.

Choose the execution mode deliberately:
- Foreground (background omitted or false): use when you need the answer before continuing. The task call waits and returns the subagent's answer as its result. Multiple independent foreground task calls issued together wait for all of their results.
- Background (background=true): use only when you can continue useful independent work without the answer. The call returns an id immediately; completion is delivered automatically as a subagent_event and wakes you in a later turn.

Never use sleep, schedule, or repeated get_subagent_result calls to wait for subagents. get_subagent_result is a diagnostic snapshot only.

The subagent has ZERO context from your conversation. Write a complete briefing: what to do and why, what you already know or ruled out, file paths and constraints, whether to MODIFY code or RESEARCH only, and what to return.

Bad: "Look into the auth bug and fix it"
Good: "In internal/auth/service.go, refreshToken() at line 45 deletes the token before persist() completes. Verify this race by tracing the call sequence in service.go and store.go. Return root cause with file:line references. Do not edit code."

Default timeout is 5 minutes. For longer tasks (test suites, large refactors), set the timeout parameter. Each new subagent starts with zero parent context. To continue an existing subagent while preserving its full context, use send_to_subagent with the id returned by task.%s`, typeList.String(), modelList)
}

func (t *taskTool) Parameters() json.RawMessage {
	typeNames := make([]string, len(t.subagentTypes))
	for i, info := range t.subagentTypes {
		typeNames[i] = fmt.Sprintf("%q", info.Name)
	}

	enumStr := "[" + strings.Join(typeNames, ", ") + "]"

	return json.RawMessage(fmt.Sprintf(`{
		"type": "object",
		"properties": {
			"prompt": {
				"type": "string",
				"description": "The detailed task description for the subagent"
			},
			"description": {
				"type": "string",
				"description": "A short (3-5 word) description of the task"
			},
			"subagent_type": {
				"type": "string",
				"enum": %s,
				"description": "The type of subagent to spawn"
			},
			"model": {
				"type": "string",
				"description": "Optional tagged model ID from the available candidates. If omitted, inherits current session model."
			},
			"timeout": {
				"type": "integer",
				"description": "Timeout in seconds (default: 300). Applies to blocking tasks only."
			},
			"background": {
				"type": "boolean",
				"description": "When false or omitted, wait for the answer before continuing. Set true only when you can continue useful independent work without the answer: the call returns the subagent id immediately, and completion is delivered automatically and wakes the parent. Never use sleep or get_subagent_result polling to wait for it."
			}
		},
		"required": ["prompt", "description", "subagent_type"]
	}`, enumStr))
}

func (t *taskTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	var p TaskParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid parameters: %w", err)
	}

	if err := t.validateParams(p); err != nil {
		return nil, err
	}

	if p.Background {
		return t.executeBackground(ctx, p)
	}

	return t.executeBlocking(ctx, p)
}

// executeBackground spawns a child via the daemon and returns its id immediately.
// Completion arrives later as a synthetic subagent_event pair.
func (t *taskTool) executeBackground(ctx context.Context, p TaskParams) (*tool.Result, error) {
	if t.spawner == nil {
		return nil, errors.New("background subagents are not available in this context")
	}

	callID := tool.CallIDFromContext(ctx)
	if callID == "" {
		return nil, errors.New("background task requires a tool call id")
	}

	res, err := t.spawner.Spawn(ctx, spawnRequest{
		ParentID:   t.parentID,
		AgentType:  p.SubagentType,
		AgentModel: t.agentModel(p.SubagentType),
		Prompt:     p.Prompt,
		Model:      p.Model,
		Blocking:   false,
		TaskCallID: callID,
		TimeoutSec: p.Timeout,
	})
	if err != nil {
		return nil, fmt.Errorf("spawn background subagent: %w", err)
	}

	output := fmt.Sprintf(
		"Launched background subagent #%d (%s). Continue useful independent work. "+
			"Its completion will be delivered automatically and wake this session; do not use sleep or poll get_subagent_result to wait for it. "+
			"Use send_to_subagent(id=%d, ...) only to add follow-up work.",
		res.ChildID, p.SubagentType, res.ChildID,
	)

	return &tool.Result{
		Title:  p.Description,
		Output: output + taskMetadata(res.ChildID),
		Metadata: map[string]any{
			"subagentType": p.SubagentType,
			"id":           res.ChildID,
			"background":   true,
		},
	}, nil
}

// executeBlocking spawns a child and suspends the parent: the loop yields its
// run-slot (no priority-inversion deadlock) and the child's completion fills
// this exact task tool_use on resume via the durable child-link contract.
func (t *taskTool) executeBlocking(ctx context.Context, p TaskParams) (*tool.Result, error) {
	if t.spawner == nil {
		return nil, errors.New("subagents are not available in this context")
	}

	callID := tool.CallIDFromContext(ctx)
	if callID == "" {
		return nil, errors.New("blocking task requires a tool call id")
	}

	// Resume idempotency: if this exact task call already spawned a child that is
	// still in flight, re-suspend without re-forking (Decision 14).
	if callID != "" {
		pending, err := t.spawner.LinkPending(ctx, t.parentID, callID)
		if err != nil {
			return nil, fmt.Errorf("check pending subagent: %w", err)
		}

		if pending {
			return nil, tool.ErrSuspend
		}
	}

	if _, err := t.spawner.Spawn(ctx, spawnRequest{
		ParentID:   t.parentID,
		AgentType:  p.SubagentType,
		AgentModel: t.agentModel(p.SubagentType),
		Prompt:     p.Prompt,
		Model:      p.Model,
		Blocking:   true,
		TaskCallID: callID,
		TimeoutSec: p.Timeout,
	}); err != nil {
		return nil, fmt.Errorf("spawn subagent: %w", err)
	}

	// Suspend: the parent's loop exits (releasing its slot); the child's
	// completion is injected as this task tool_use's result on resume.
	return nil, tool.ErrSuspend
}

func (t *taskTool) validateParams(p TaskParams) error {
	if p.Prompt == "" {
		return errors.New("prompt is required")
	}

	if p.SubagentType == "" {
		return errors.New("subagent_type is required")
	}

	if p.Model != "" && !t.isCandidateModel(p.Model) {
		return fmt.Errorf("model %q is not an advertised tagged subagent candidate", p.Model)
	}

	for _, info := range t.subagentTypes {
		if info.Name == p.SubagentType {
			return nil
		}
	}

	typeNames := make([]string, 0, len(t.subagentTypes))
	for _, info := range t.subagentTypes {
		typeNames = append(typeNames, info.Name)
	}

	return fmt.Errorf("invalid subagent_type: %s (available: %s)", p.SubagentType, strings.Join(typeNames, ", "))
}

func (t *taskTool) isCandidateModel(id string) bool {
	for _, model := range t.modelCatalog {
		if model.ID == id {
			return true
		}
	}

	return false
}

// agentModel returns the agent type's configured model override, or "" when the
// type has none (the daemon then falls back to task param / parent model).
func (t *taskTool) agentModel(subagentType string) string {
	cfg, ok := t.set.Get(registry.AgentType(subagentType))
	if !ok {
		return ""
	}

	return cfg.Model
}

func taskMetadata(id int64) string {
	return fmt.Sprintf("\n\n<task_metadata>\nid: %d\n</task_metadata>", id)
}
