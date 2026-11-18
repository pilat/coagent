package builtin

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/tool"
)

type directOnlyScriptedTool struct{ *scriptedTool }

func (*directOnlyScriptedTool) DirectOnly() {}

func TestBatchToolRejectsDirectOnlyBeforeAnyCallRuns(t *testing.T) {
	t.Parallel()
	first := &scriptedTool{id: "first", result: &tool.Result{Output: "first"}}
	page := &directOnlyScriptedTool{scriptedTool: &scriptedTool{
		id: "page", result: &tool.Result{Output: "page"},
	}}
	last := &scriptedTool{id: "last", result: &tool.Result{Output: "last"}}
	registry := tool.NewRegistry()
	registry.Register(first)
	registry.Register(page)
	registry.Register(last)

	result, err := NewBatchTool(registry).Execute(t.Context(), json.RawMessage(batchCalls("first", "page", "last")))
	assert.Zero(t, first.started.Load())
	assert.Zero(t, page.started.Load())
	assert.Zero(t, last.started.Load())
	require.EqualError(t, err, "call 2: page must be called directly, one call per session, not inside batch")
	assert.Nil(t, result)
}
