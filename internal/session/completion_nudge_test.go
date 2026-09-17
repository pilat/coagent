package session

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pilat/coagent/internal/todo"
)

func TestRenderCompletionNudge_OmitsTodoClauseWhenNoneOpen(t *testing.T) {
	nudge := renderCompletionNudge([]*todo.Item{
		{ID: "1", Content: "done work", Status: todo.StatusCompleted},
		{ID: "2", Content: "dropped work", Status: todo.StatusCancelled},
	})
	assert.NotContains(t, nudge, "todo", "no open items means no todo wording")
	assert.NotContains(t, nudge, "done work")
	assert.Contains(t, nudge, "why you are stopping")
}

func TestRenderCompletionNudge_NamesOnlyOpenItems(t *testing.T) {
	nudge := renderCompletionNudge([]*todo.Item{
		{ID: "1", Content: "pending work", Status: todo.StatusPending},
		{ID: "2", Content: "active work", Status: todo.StatusInProgress},
		{ID: "3", Content: "done work", Status: todo.StatusCompleted},
	})
	assert.Contains(t, nudge, "pending work")
	assert.Contains(t, nudge, "active work")
	assert.NotContains(t, nudge, "done work")
	assert.NotContains(t, nudge, "OK", "the protocol never requires exact acknowledgement text")
}

func TestRenderCompletionNudge_EmptyListHasNoTodoClause(t *testing.T) {
	nudge := renderCompletionNudge(nil)
	assert.NotContains(t, nudge, "todo")
	assert.Contains(t, nudge, "Re-check the original request")
}
