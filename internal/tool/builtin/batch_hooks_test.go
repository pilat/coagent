package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
)

type refusingBatchHooks struct {
	calls []string
}

func (h *refusingBatchHooks) BeforeCall(_ context.Context, call tool.Call) tool.HookVerdict {
	h.calls = append(h.calls, call.Name)

	return tool.HookVerdict{Refusal: "operator guard refused this command"}
}

func (h *refusingBatchHooks) AfterCall(context.Context, tool.Call, *tool.Result) []string {
	return nil
}

func TestBatchNestedCallSeesCallHooks(t *testing.T) {
	workDir := t.TempDir()
	stack, err := BuildStack(context.Background(), StackConfig{
		WorkDir: workDir, Loader: loader.New(), Todo: todo.New(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = stack.Close() })
	hooks := &refusingBatchHooks{}
	ctx := tool.WithCallHooks(context.Background(), hooks)
	result, err := stack.Registry.Get(tool.IDBatch).Execute(ctx, json.RawMessage(
		`{"calls":[{"tool":"bash","params":{"command":"touch should-not-exist"}}]}`,
	))
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "operator guard refused this command")
	assert.Equal(t, []string{"bash"}, hooks.calls)
	_, err = os.Stat(filepath.Join(workDir, "should-not-exist"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestBatchCarriesNestedHookFeedback(t *testing.T) {
	first := &scriptedTool{
		id: "first", result: &tool.Result{Output: "one"}, parallelSafe: true,
	}
	second := &scriptedTool{
		id: "second", result: &tool.Result{Output: "two", IsError: true}, parallelSafe: true,
	}
	registry := tool.NewRegistry()
	registry.Register(first)
	registry.Register(second)
	batch := NewBatchTool(registry)
	params := json.RawMessage(batchCalls("first", "second"))
	baseline, err := batch.Execute(context.Background(), params)
	require.NoError(t, err)
	require.NotNil(t, baseline)
	assert.Empty(t, baseline.HookFeedback)
	first.result.HookFeedback = []string{"first context", "first feedback"}
	second.result.HookFeedback = []string{"second feedback"}
	withFeedback, err := batch.Execute(context.Background(), params)
	require.NoError(t, err)
	require.NotNil(t, withFeedback)
	want := *baseline
	want.HookFeedback = []string{"first context", "first feedback", "second feedback"}
	assert.Equal(t, &want, withFeedback)
	assert.Equal(t, baseline.Output, withFeedback.Output)
}
