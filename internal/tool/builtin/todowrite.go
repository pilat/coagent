package builtin

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
)

const todoWriteDescription = `Creates and manages a structured task list for complex coding sessions. This helps track progress and organize work.

## When to Use This Tool

Use this tool proactively in these scenarios:
1. Complex multistep tasks (3+ distinct steps)
2. Non-trivial tasks requiring careful planning
3. User explicitly requests a todo list
4. User provides multiple tasks (numbered or comma-separated)
5. After receiving new instructions - immediately capture requirements
6. After completing a task - mark complete and add follow-ups
7. When starting a new task, mark it in_progress

## When NOT to Use This Tool

Skip using this tool when:
1. Only a single, straightforward task
2. The task is trivial and tracking provides no benefit
3. The task can be completed in less than 3 trivial steps
4. The task is purely conversational or informational

## Task States

- pending: Task not yet started
- in_progress: Currently working on (limit to ONE task at a time)
- completed: Task finished successfully
- cancelled: Task no longer needed

## Best Practices

- Update task status in real-time as you work
- Mark tasks complete IMMEDIATELY after finishing
- Only have ONE task in_progress at any time
- Complete current tasks before starting new ones
- Create specific, actionable items
- Break complex tasks into smaller, manageable steps`

var _ tool.Tool = (*todoWriteTool)(nil)

type todoWriteParams struct {
	Items []todoItem `json:"items"`
}

type todoItem struct {
	ID       string `json:"id,omitempty"`
	Content  string `json:"content"`
	Status   string `json:"status,omitempty"`
	Priority string `json:"priority,omitempty"`
}

type todoWriteTool struct {
	store todo.Service
}

func newTodoWriteTool(store todo.Service) *todoWriteTool {
	return &todoWriteTool{store: store}
}

func (t *todoWriteTool) ID() string          { return "todowrite" }
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

	items := make([]*todo.Item, len(p.Items))
	for i, item := range p.Items {
		items[i] = &todo.Item{
			ID:       item.ID,
			Content:  item.Content,
			Status:   todo.Status(item.Status),
			Priority: todo.Priority(item.Priority),
		}
	}

	t.store.Replace(items)

	return &tool.Result{
		Title:  "Todo List Updated",
		Output: fmt.Sprintf("Todo list updated with %d items.", len(p.Items)),
		Metadata: map[string]any{
			metaKeyCount: len(p.Items),
		},
	}, nil
}
