package sessionprompt

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pilat/coagent/internal/todo"
)

func TestRenderCompletionNudge_OmitsTodoClauseWhenNoneOpen(t *testing.T) {
	nudge := RenderCompletionNudge([]*todo.Item{
		{ID: "1", Content: "done work", Status: todo.StatusCompleted},
		{ID: "2", Content: "dropped work", Status: todo.StatusCancelled},
	})
	assert.NotContains(t, nudge, "todo", "no open items means no todo wording")
	assert.NotContains(t, nudge, "done work")
	assert.NotContains(t, nudge, "why you are stopping", "a stop justification displaces the answer")
	assert.Contains(t, nudge, "your previous answer is delivered as is and this confirmation is never shown")
	assert.Contains(t, nudge, "next text-only response ends the task")
	assert.Contains(t, nudge, "delivered to the user in place of your previous answer")
	assert.Contains(t, nudge, "complete answer to the original request")
	assert.Contains(t, nudge, "keeping every finding and caveat from your previous answer")
	assert.Contains(t, nudge, "not as a report of this re-check")
}

func TestRenderCompletionNudge_NamesOnlyOpenItems(t *testing.T) {
	nudge := RenderCompletionNudge([]*todo.Item{
		{ID: "1", Content: "pending work", Status: todo.StatusPending},
		{ID: "2", Content: "active work", Status: todo.StatusInProgress},
		{ID: "3", Content: "done work", Status: todo.StatusCompleted},
	})
	assert.Contains(t, nudge, "pending work")
	assert.Contains(t, nudge, "active work")
	assert.Contains(t, nudge, "next text-only response ends the task")
	assert.Contains(t, nudge, "keeping every finding and caveat from your previous answer")
	assert.Contains(t, nudge, "not as a report of this re-check")
	assert.NotContains(t, nudge, "done work")
	assert.NotContains(t, nudge, "OK", "the protocol never requires exact acknowledgement text")
}

func TestRenderCompletionNudge_EmptyListHasNoTodoClause(t *testing.T) {
	nudge := RenderCompletionNudge(nil)
	assert.NotContains(t, nudge, "todo")
	assert.Contains(t, nudge, "Re-check the original request")
}
