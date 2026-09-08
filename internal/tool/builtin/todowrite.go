//nolint:wrapcheck // Replacement validation errors are already tool-facing.; nosemgrep: semgrep.coagent-no-preamble-before-package
package builtin

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
)

const todoWriteDescription = `Replaces the session's complete todo list. For large work with multiple deliverables, packages, or dependent phases, create the list before implementation and use it to guide the work. Also use it when the user requests a plan. Small, straightforward tasks need no list.

Each item should describe a concrete outcome, not an individual tool call. Send the full list on every update: omitted items are removed. Preserve existing item IDs when updating them. Send items=[] to clear the list when it is no longer useful.

Task states:

- pending: Task not yet started
- in_progress: Currently working on (limit to ONE task at a time)
- completed: Task finished successfully
- cancelled: Task no longer needed

Mark the current item in_progress before starting it. Mark it completed immediately after the outcome and required verification are done, then choose the next pending item. Delegated work stays unfinished until its result is received and reviewed. Revise the list when requirements, findings, or blockers change the plan. Before the final response, reconcile the list with the actual result; keep unresolved work visible. Do not invent follow-up work merely to keep the list populated.`

var _ tool.Tool = (*todoWriteTool)(nil)

type todoWriteParams struct {
	Items []todoItem `json:"items"`
}

type todoItem struct {
	ID       *string `json:"id,omitempty"`
	Content  string  `json:"content"`
	Status   string  `json:"status,omitempty"`
	Priority string  `json:"priority,omitempty"`
}

type todoWriteTool struct {
	store   todo.Service
	replace TodoReplacement
}

type TodoReplacement interface {
	ReplaceTodo(ctx context.Context, callID string, items []TodoReplacementItem) ([]*todo.Item, error)
}

type TodoReplacementItem struct {
	ID       *string
	Content  string
	Status   todo.Status
	Priority todo.Priority
}

func newTodoWriteTool(store todo.Service, replace TodoReplacement) *todoWriteTool {
	return &todoWriteTool{store: store, replace: replace}
}

func (t *todoWriteTool) ID() string          { return "todowrite" }
func (t *todoWriteTool) ParallelSafe() bool  { return false }
func (t *todoWriteTool) Description() string { return todoWriteDescription }

func (t *todoWriteTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"items": {
				"type": "array",
				"description": "The complete list of todo items to set",
				"items": {
					"type": "object",
					"properties": {
						"id": {
							"type": "string",
							"description": "Optional ID to preserve (for updates)"
						},
						"content": {
							"type": "string",
							"description": "Task description"
						},
						"status": {
							"type": "string",
							"enum": ["pending", "in_progress", "completed", "cancelled"],
							"description": "Task status (default: pending)"
						},
						"priority": {
							"type": "string",
							"enum": ["high", "medium", "low"],
							"description": "Task priority (default: medium)"
						}
					},
					"required": ["content"]
				}
			}
		},
		"required": ["items"]
	}`)
}

func (t *todoWriteTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	var p todoWriteParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid parameters: %w", err)
	}

	replacements := make([]TodoReplacementItem, len(p.Items))
	for i, item := range p.Items {
		replacements[i] = TodoReplacementItem{
			ID:       item.ID,
			Content:  item.Content,
			Status:   todo.Status(item.Status),
			Priority: todo.Priority(item.Priority),
		}
	}

	if t.replace != nil {
		if _, err := t.replace.ReplaceTodo(ctx, tool.CallIDFromContext(ctx), replacements); err != nil {
			return nil, err
		}
	} else {
		items := make([]*todo.Item, len(replacements))
		for i, item := range replacements {
			var itemID string
			if item.ID != nil {
				itemID = *item.ID
			}

			items[i] = &todo.Item{ID: itemID, Content: item.Content, Status: item.Status, Priority: item.Priority}
		}

		t.store.Replace(items)
	}

	return &tool.Result{
		Title:  "Todo List Updated",
		Output: fmt.Sprintf("Todo list updated with %d items.", len(p.Items)),
		Metadata: map[string]any{
			metaKeyCount: len(p.Items),
		},
	}, nil
}
