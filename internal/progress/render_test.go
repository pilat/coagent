package progress

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestRenderCompact_TruthfulFallbackAndRedaction(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	snapshot := Snapshot{
		ObservedAt:          now,
		RuntimeState:        "running",
		Context:             Context{Available: false},
		Lifetime:            Usage{Available: false},
		Todos:               []TodoItem{{ID: "a", Content: "inspect secret-value", Status: "pending"}},
		LatestModelProgress: "using secret-value",
	}

	rendered := RenderCompact(snapshot, func(value string) string {
		return strings.ReplaceAll(value, "secret-value", "[REDACTED]")
	})
	assert.Contains(t, rendered, "Context: unavailable")
	assert.Contains(t, rendered, "Lifetime usage: unavailable")
	assert.Contains(t, rendered, "[REDACTED]")
	assert.NotContains(t, rendered, "secret-value")
}

func TestRenderCompact_NoTodoAndCappedUnicodeExcerpt(t *testing.T) {
	t.Parallel()

	snapshot := Snapshot{LatestModelProgress: strings.Repeat("界", 513)}
	rendered := RenderCompact(snapshot, nil)

	assert.Contains(t, rendered, "no TODO is declared")
	assert.Contains(t, rendered, strings.Repeat("界", 512)+"…")
	assert.NotContains(t, rendered, strings.Repeat("界", 513))
}

func TestRenderFooter_OmitsAbsentTodo(t *testing.T) {
	t.Parallel()

	assert.Empty(t, RenderFooter(Snapshot{}, nil))

	todo := RenderFooter(Snapshot{
		Todos: []TodoItem{{ID: "a", Content: "ship change", Status: "in_progress"}},
	}, nil)
	assert.Contains(t, todo, "ship change")

	budget := RenderFooter(Snapshot{Budget: &Budget{State: "armed", Generation: 1}}, nil)
	assert.NotContains(t, budget, "TODO")
	assert.Contains(t, budget, "Budget: armed")
}
