package sessionbuild

import (
	"context"
	"fmt"
	"time"

	"github.com/pilat/coagent/internal/sessionprompt"
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
	input []todo.ReplacementItem,
) ([]*todo.Item, error) {
	items, err := sessionprompt.NormalizeTodoReplacement(callID, input)
	if err != nil {
		return nil, fmt.Errorf("replace todo: %w", err)
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
