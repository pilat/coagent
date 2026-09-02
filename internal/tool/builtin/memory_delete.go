package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/pilat/coagent/internal/memory"
	"github.com/pilat/coagent/internal/tool"
)

var _ tool.Tool = (*memoryDeleteTool)(nil)

// memoryDeleteParams are the parameters for the memory_delete tool.
type memoryDeleteParams struct {
	ID int64 `json:"id"`
}

type memoryDeleteTool struct {
	store     memory.CuratedStore
	projectID int64
	onChanged func(context.Context)
}

func NewMemoryDeleteTool(store memory.CuratedStore, projectID int64, onChanged func(context.Context)) tool.Tool {
	return &memoryDeleteTool{store: store, projectID: projectID, onChanged: onChanged}
}

func (t *memoryDeleteTool) ID() string         { return "memory_delete" }
func (t *memoryDeleteTool) ParallelSafe() bool { return false }

func (t *memoryDeleteTool) Description() string {
	return `Delete a curated memory by ID.

Use when a memory is outdated or needs to be replaced.`
}

func (t *memoryDeleteTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"id": {
				"type": "integer",
				"description": "The memory ID to delete."
			}
		},
		"required": ["id"]
	}`)
}

func (t *memoryDeleteTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	var p memoryDeleteParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid parameters: %w", err)
	}

	if p.ID == 0 {
		return nil, errors.New("id is required")
	}

	if err := t.store.DeleteMemory(ctx, t.projectID, p.ID); err != nil {
		return nil, fmt.Errorf("delete memory: %w", err)
	}

	if t.onChanged != nil {
		t.onChanged(ctx)
	}

	// Show remaining memories
	remaining, err := t.store.ListMemoryTexts(ctx, t.projectID)
	if err != nil {
		return &tool.Result{
			Title:  "Memory delete",
			Output: fmt.Sprintf("Deleted memory %d. (Could not list remaining: %v)", p.ID, err),
		}, nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Deleted memory %d. Remaining memories (%d):\n", p.ID, len(remaining))

	for _, m := range remaining {
		fmt.Fprintf(&sb, "- [%d] %s\n", m.ID, m.Text)
	}

	if len(remaining) == 0 {
		sb.WriteString("(none)")
	}

	return &tool.Result{
		Title:  "Memory delete",
		Output: sb.String(),
	}, nil
}
