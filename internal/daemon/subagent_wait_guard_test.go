package daemon

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/tool"
)

type waitGuardTool struct{ calls int }

func (t *waitGuardTool) ID() string                  { return tool.IDSleep }
func (t *waitGuardTool) ParallelSafe() bool          { return false }
func (t *waitGuardTool) Description() string         { return "sleep" }
func (t *waitGuardTool) Parameters() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *waitGuardTool) Execute(context.Context, json.RawMessage) (*tool.Result, error) {
	t.calls++
	return &tool.Result{Output: "slept"}, nil
}

func TestSubagentWaitGuardRejectsSleepUntilCompletionDelivered(t *testing.T) {
	inner := &waitGuardTool{}
	pending := true
	guard := &subagentWaitGuard{
		inner:      inner,
		hasPending: func(context.Context) (bool, error) { return pending, nil },
	}

	_, err := guard.Execute(t.Context(), nil)
	require.Error(t, err)
	require.ErrorContains(t, err, "subagent will wake the session automatically")
	assert.Zero(t, inner.calls, "the sleep side effect must not be staged")

	pending = false
	result, err := guard.Execute(t.Context(), nil)
	require.NoError(t, err)
	assert.Equal(t, "slept", result.Output)
	assert.Equal(t, 1, inner.calls)
}
