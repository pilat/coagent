package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckpointControlKeepsQueuedCommandSeparateFromActiveAttempt(t *testing.T) {
	var control checkpointControl
	control.queue(PendingInput{ID: 1}, "first focus")
	first := control.claim()
	require.NotNil(t, first)
	require.NotNil(t, first.input)
	assert.Equal(t, int64(1), first.input.ID)
	assert.Equal(t, "first focus", control.focus())

	control.queue(PendingInput{ID: 2}, "second focus")
	assert.True(t, control.requested())
	assert.Same(t, first, control.claim(), "the active attempt keeps its claim")
	assert.Equal(t, "first focus", control.focus())

	control.finish(first)
	second := control.claim()
	require.NotNil(t, second)
	require.NotNil(t, second.input)
	assert.Equal(t, int64(2), second.input.ID)
	assert.Equal(t, "second focus", control.focus())
	control.finish(second)
	assert.False(t, control.requested())
}

func TestCheckpointControlDoesNotUseQueuedFocusInAutomaticAttempt(t *testing.T) {
	var control checkpointControl
	assert.Nil(t, control.claim())
	control.queue(PendingInput{ID: 2}, "later focus")
	assert.Empty(t, control.focus(), "an automatic attempt has no claimed focus")

	claimed := control.claim()
	require.NotNil(t, claimed)
	assert.Equal(t, "later focus", control.focus())
}
