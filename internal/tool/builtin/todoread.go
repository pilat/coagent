package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
)

const todoReadDescription = `Reads the current todo list for the session. Use this tool proactively and frequently to ensure you are aware of the task status.

Use this tool:
- At the beginning of conversations to see what's pending
- Before starting new tasks to prioritize work
- When the user asks about previous tasks or plans
- Whenever you're uncertain what to do next
- After completing tasks to update your understanding
- After every few messages to ensure you're on track

Returns:
- List of todos sorted by priority (high first), then creation time
- Each item has: id, content, status, priority, created_at, updated_at`

var _ tool.Tool = (*todoReadTool)(nil)

type todoReadTool struct {
	store todo.Service
}

func newTodoReadTool(store todo.Service) *todoReadTool {
	return &todoReadTool{store: store}
}

func (t *todoReadTool) ID() string          { return "todoread" }
func (t *todoReadTool) Description() string { return todoReadDescription }

func (t *todoReadTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {},
		"required": []
	}`)
}

func (t *todoReadTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	items := t.store.List()

	sort.Slice(items, func(i, j int) bool {
		pi, pj := priorityOrder(items[i].Priority), priorityOrder(items[j].Priority)
		if pi != pj {
			return pi < pj
		}

		return items[i].CreatedAt.Before(items[j].CreatedAt)
	})

	output, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal todo list: %w", err)
	}

	return &tool.Result{
		Title:  "Todo List",
		Output: string(output),
		Metadata: map[string]any{
			metaKeyCount: len(items),
		},
	}, nil
}

func priorityOrder(p todo.Priority) int {
	switch p {
	case todo.PriorityHigh:
		return 0
	case todo.PriorityMedium:
		return 1
	case todo.PriorityLow:
		return 2
	default:
		return 3
	}
}
