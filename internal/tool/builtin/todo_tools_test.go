package builtin

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/todo"
)

func TestTodoWriteReplacesTheWholeList(t *testing.T) {
	store := todo.New()
	store.Add("stale", todo.PriorityHigh)

	raw, err := json.Marshal(todoWriteParams{Items: []todoItem{
		{ID: "a", Content: "first", Status: "in_progress", Priority: "high"},
		{Content: "second"},
	}})
	require.NoError(t, err)

	result, err := newTodoWriteTool(store).Execute(context.Background(), raw)
	require.NoError(t, err)

	assert.Equal(t, "Todo list updated with 2 items.", result.Output)
	assert.Equal(t, 2, result.Metadata[metaKeyCount])
	assert.Equal(t, 2, store.Count(), "the previous list must be gone")

	kept := store.Get("a")
	require.NotNil(t, kept)
	assert.Equal(t, todo.StatusInProgress, kept.Status)
	assert.Equal(t, todo.PriorityHigh, kept.Priority)
}

func TestTodoWriteRejectsMalformedParams(t *testing.T) {
	_, err := newTodoWriteTool(todo.New()).Execute(context.Background(), json.RawMessage(`{`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid parameters")
}

func TestTodoReadSortsByPriorityThenAge(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	store := todo.New()
	store.Replace([]*todo.Item{
		{ID: "low", Content: "low", Priority: todo.PriorityLow, CreatedAt: base},
		{ID: "unknown", Content: "unknown", Priority: todo.Priority("weird"), CreatedAt: base},
		{ID: "high-late", Content: "high late", Priority: todo.PriorityHigh, CreatedAt: base.Add(time.Minute)},
		{ID: "high-early", Content: "high early", Priority: todo.PriorityHigh, CreatedAt: base},
		{ID: "medium", Content: "medium", Priority: todo.PriorityMedium, CreatedAt: base},
	})

	result, err := newTodoReadTool(store).Execute(context.Background(), json.RawMessage(`{}`))
	require.NoError(t, err)

	var items []todo.Item
	require.NoError(t, json.Unmarshal([]byte(result.Output), &items))

	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}

	assert.Equal(t, []string{"high-early", "high-late", "medium", "low", "unknown"}, ids)
	assert.Equal(t, 5, result.Metadata[metaKeyCount])
}

func TestTodoReadOnEmptyList(t *testing.T) {
	result, err := newTodoReadTool(todo.New()).Execute(context.Background(), json.RawMessage(`{}`))
	require.NoError(t, err)

	assert.Equal(t, "[]", result.Output)
	assert.Equal(t, 0, result.Metadata[metaKeyCount])
}

func TestPriorityOrder(t *testing.T) {
	assert.Equal(t, 0, priorityOrder(todo.PriorityHigh))
	assert.Equal(t, 1, priorityOrder(todo.PriorityMedium))
	assert.Equal(t, 2, priorityOrder(todo.PriorityLow))
	assert.Equal(t, 3, priorityOrder(todo.Priority("")))
}
