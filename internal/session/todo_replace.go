package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool/builtin"
)

type todoReplacement struct {
	memory todo.Service
}

var _ builtin.TodoReplacement = (*todoReplacement)(nil)

func (r *todoReplacement) ReplaceTodo(
	_ context.Context,
	callID string,
	input []builtin.TodoReplacementItem,
) ([]*todo.Item, error) {
	items, err := normalizeTodoReplacement(callID, input)
	if err != nil {
		return nil, err
	}

	// The replacement schema has no timestamp field: generated timestamps must
	// land in the durable JSON too, or restart projections lose their ordering inputs.
	now := time.Now().UTC()

	for _, item := range items {
		if item.CreatedAt.IsZero() {
			item.CreatedAt = now
		}

		if item.UpdatedAt.IsZero() {
			item.UpdatedAt = now
		}
	}

	r.memory.Replace(items)

	return items, nil
}

func normalizeTodoReplacement(callID string, input []builtin.TodoReplacementItem) ([]*todo.Item, error) {
	if callID == "" {
		return nil, errors.New("todo replacement requires tool call identity")
	}

	items := make([]*todo.Item, len(input))

	seen := make(map[string]struct{}, len(input))
	for i, candidate := range input {
		id := fmt.Sprintf("%s:%d", callID, i)
		if candidate.ID != nil {
			id = *candidate.ID
			if id == "" {
				return nil, fmt.Errorf("todo item %d has an empty id", i+1)
			}
		}

		if _, duplicate := seen[id]; duplicate {
			return nil, fmt.Errorf("todo item %d duplicates id %q", i+1, id)
		}

		seen[id] = struct{}{}

		content := strings.TrimSpace(candidate.Content)
		if content == "" {
			return nil, fmt.Errorf("todo item %d has blank content", i+1)
		}

		status := candidate.Status
		if status == "" {
			status = todo.StatusPending
		}

		priority := candidate.Priority
		if priority == "" {
			priority = todo.PriorityMedium
		}

		items[i] = &todo.Item{ID: id, Content: content, Status: status, Priority: priority}
	}

	return items, nil
}
