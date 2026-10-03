package sessionprompt

import (
	"errors"
	"fmt"
	"strings"

	"github.com/pilat/coagent/internal/todo"
)

// NormalizeTodoReplacement assigns stable call-scoped identities and validates replacements.
func NormalizeTodoReplacement(callID string, input []todo.ReplacementItem) ([]*todo.Item, error) {
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
