package sessionlifecycle

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQueuePreservesOrderAcrossSelectionAndRemoval(t *testing.T) {
	t.Parallel()

	queue := NewQueue[int]()
	for _, value := range []int{1, 2, 3, 4} {
		queue.Push(value)
	}

	selected, ok := queue.PopFirst(func(value int) bool { return value%2 == 0 })
	require.True(t, ok)
	assert.Equal(t, 2, selected)

	queue.Remove(func(value int) bool { return value == 3 })
	first, ok := queue.PopFirst(func(int) bool { return true })
	require.True(t, ok)
	assert.Equal(t, 1, first)
	second, ok := queue.PopFirst(func(int) bool { return true })
	require.True(t, ok)
	assert.Equal(t, 4, second)
}

func TestQueuePushUniqueKeepsOneMatchingValue(t *testing.T) {
	t.Parallel()

	queue := NewQueue[int]()
	require.True(t, queue.PushUnique(1, func(left, right int) bool { return left == right }))
	assert.False(t, queue.PushUnique(1, func(left, right int) bool { return left == right }))
	assert.Equal(t, 1, queue.Len())
}
