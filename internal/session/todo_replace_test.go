package session

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool/builtin"
)

type todoReplacementStore struct {
	mockSessionStore
	encoded json.RawMessage
	err     error
}

func (s *todoReplacementStore) UpdateSessionTodoItems(
	_ context.Context,
	_ int64,
	items json.RawMessage,
) error {
	if s.err != nil {
		return s.err
	}
	s.encoded = append(s.encoded[:0], items...)

	return nil
}

func TestTodoReplacement_PersistsEmptyAndDeterministicIDs(t *testing.T) {
	store := &todoReplacementStore{}
	memory := todo.New()
	memory.Add("stale", todo.PriorityHigh)
	replacement := &todoReplacement{store: store, sessionID: 7, memory: memory}

	items, err := replacement.ReplaceTodo(t.Context(), "call-1", []builtin.TodoReplacementItem{
		{Content: "first", Status: todo.StatusInProgress},
		{Content: "second"},
	})
	require.NoError(t, err)
	assert.Equal(t, "call-1:0", items[0].ID)
	assert.Equal(t, "call-1:1", items[1].ID)
	assert.NotNil(t, memory.Get(items[0].ID))

	_, err = replacement.ReplaceTodo(t.Context(), "call-2", nil)
	require.NoError(t, err)
	assert.JSONEq(t, `[]`, string(store.encoded))
	assert.Empty(t, memory.List())
}

func TestTodoReplacement_StoreFailureLeavesMemoryUntouched(t *testing.T) {
	store := &todoReplacementStore{err: errors.New("write failed")}
	memory := todo.New()
	kept := memory.Add("kept", todo.PriorityHigh)
	replacement := &todoReplacement{store: store, sessionID: 7, memory: memory}

	_, err := replacement.ReplaceTodo(t.Context(), "call-1", []builtin.TodoReplacementItem{{Content: "new"}})
	require.ErrorContains(t, err, "persist todo replacement")
	assert.NotNil(t, memory.Get(kept.ID))
}

// The durable JSON is the restart projection's only ordering input, so
// generated timestamps must be persisted, not just held in memory.
func TestTodoReplacement_PersistsGeneratedTimestamps(t *testing.T) {
	store := &todoReplacementStore{}
	replacement := &todoReplacement{store: store, sessionID: 7, memory: todo.New()}

	_, err := replacement.ReplaceTodo(t.Context(), "call-1", []builtin.TodoReplacementItem{
		{Content: "first"},
		{ID: &[]string{"kept"}[0], Content: "kept"},
	})
	require.NoError(t, err)

	var persisted []*todo.Item
	require.NoError(t, json.Unmarshal(store.encoded, &persisted))
	require.Len(t, persisted, 2)
	assert.False(t, persisted[0].CreatedAt.IsZero(), "new item needs a durable CreatedAt")
	assert.False(t, persisted[0].UpdatedAt.IsZero(), "new item needs a durable UpdatedAt")
	assert.False(t, persisted[1].CreatedAt.IsZero(), "explicitly identified item still gets a durable CreatedAt")
}
