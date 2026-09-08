package builtin

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
)

const todoReadDescription = `Reads the current todo list for the session. Use when you need the saved plan or item IDs and they are not already known from the current context. Do not re-read an unchanged list after each update or every few messages.

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
func (t *todoReadTool) ParallelSafe() bool  { return true }
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

	todo.SortCanonical(items)

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
