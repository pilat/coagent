package session

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockCompactor struct {
	requestedKeepRecent int
	called              bool
}

func (m *mockCompactor) RequestCompaction(keepRecentRounds int) {
	m.called = true
	m.requestedKeepRecent = keepRecentRounds
}

func TestCompactContextTool_DefaultKeepRecent(t *testing.T) {
	mc := &mockCompactor{}
	tl := newCompactContextTool(mc)

	result, err := tl.Execute(context.Background(), json.RawMessage(`{}`))
	require.NoError(t, err)

	assert.True(t, mc.called)
	assert.Equal(t, 6, mc.requestedKeepRecent, "should use default of 6")
	assert.Contains(t, result.Output, "keeping 6 recent rounds")
}

func TestCompactContextTool_CustomKeepRecent(t *testing.T) {
	mc := &mockCompactor{}
	tl := newCompactContextTool(mc)

	result, err := tl.Execute(context.Background(), json.RawMessage(`{"keep_recent_rounds": 10}`))
	require.NoError(t, err)

	assert.Equal(t, 10, mc.requestedKeepRecent)
	assert.Contains(t, result.Output, "keeping 10 recent rounds")
}

func TestCompactContextTool_MinValidation(t *testing.T) {
	mc := &mockCompactor{}
	tl := newCompactContextTool(mc)

	_, err := tl.Execute(context.Background(), json.RawMessage(`{"keep_recent_rounds": 1}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least 4")
	assert.False(t, mc.called)

	// 2 and 3 should also fail now
	_, err = tl.Execute(context.Background(), json.RawMessage(`{"keep_recent_rounds": 2}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least 4")

	_, err = tl.Execute(context.Background(), json.RawMessage(`{"keep_recent_rounds": 3}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least 4")
}

func TestCompactContextTool_MaxValidation(t *testing.T) {
	mc := &mockCompactor{}
	tl := newCompactContextTool(mc)

	_, err := tl.Execute(context.Background(), json.RawMessage(`{"keep_recent_rounds": 21}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at most 20")
	assert.False(t, mc.called)
}

func TestCompactContextTool_NilCompactor(t *testing.T) {
	tl := newCompactContextTool(nil)

	_, err := tl.Execute(context.Background(), json.RawMessage(`{}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "compactor not configured")
}

func TestCompactContextTool_ID(t *testing.T) {
	tl := newCompactContextTool(nil)
	assert.Equal(t, "compact_context", tl.ID())
}

func TestCompactContextTool_BoundaryValues(t *testing.T) {
	mc := &mockCompactor{}
	tl := newCompactContextTool(mc)

	// Exactly 4 — should work (new minimum)
	_, err := tl.Execute(context.Background(), json.RawMessage(`{"keep_recent_rounds": 4}`))
	require.NoError(t, err)
	assert.Equal(t, 4, mc.requestedKeepRecent)

	// Exactly 20 — should work
	mc.called = false
	_, err = tl.Execute(context.Background(), json.RawMessage(`{"keep_recent_rounds": 20}`))
	require.NoError(t, err)
	assert.Equal(t, 20, mc.requestedKeepRecent)
}
