package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool/builtin"
)

type todoReplacement struct {
	store     sessionstore.RuntimeStore
	sessionID int64
	memory    todo.Service
}

var _ builtin.TodoReplacement = (*todoReplacement)(nil)

func (r *todoReplacement) ReplaceTodo(
	ctx context.Context,
	callID string,
	input []builtin.TodoReplacementItem,
) ([]*todo.Item, error) {
	items, err := normalizeTodoReplacement(callID, input)
	if err != nil {
		return nil, err
	}

	encoded, err := json.Marshal(items)
	if err != nil {
		return nil, fmt.Errorf("marshal todo replacement: %w", err)
	}

	if r.store != nil {
		if err := r.store.UpdateSessionTodoItems(ctx, r.sessionID, encoded); err != nil {
			return nil, fmt.Errorf("persist todo replacement: %w", err)
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
