package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/tool"
)

type scriptedTool struct {
	id      string
	result  *tool.Result
	err     error
	started atomic.Int32
	release chan struct{}
}

var _ tool.Tool = (*scriptedTool)(nil)

func (s *scriptedTool) ID() string                  { return s.id }
func (s *scriptedTool) Description() string         { return s.id + " description" }
func (s *scriptedTool) Parameters() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (s *scriptedTool) Execute(_ context.Context, _ json.RawMessage) (*tool.Result, error) {
	s.started.Add(1)

	if s.release != nil {
		<-s.release
	}

	return s.result, s.err
}

func TestBatchToolValidatesCalls(t *testing.T) {
	registry := tool.NewRegistry()
	registry.Register(&scriptedTool{id: "ok", result: &tool.Result{Output: "done"}})
	registry.Register(&scriptedTool{id: tool.IDSetProvider, result: &tool.Result{Output: "never runs"}})
	registry.Register(NewBatchTool(registry))

	batch := NewBatchTool(registry)

	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{name: "malformed json", raw: `{`, wantErr: "invalid parameters"},
		{name: "no calls", raw: `{"calls":[]}`, wantErr: "at least one call is required"},
		{name: "nested batch", raw: batchCalls("ok", "batch"), wantErr: "call 2: nested batch is not allowed"},
		{name: "unknown tool", raw: batchCalls("ok", "ghost"), wantErr: `call 2: unknown tool "ghost"`},
		{
			// A suspending tool answers after the loop stops. Batching one would
			// report a result for work still in flight and strand the real call.
			name:    "suspending tool",
			raw:     batchCalls("ok", tool.IDSetProvider),
			wantErr: "call 2: set_provider suspends the session",
		},
		{name: "over the size cap", raw: batchCalls(repeatTool("ok", 26)...), wantErr: "maximum 25 calls allowed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := batch.Execute(context.Background(), json.RawMessage(tt.raw))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestBatchToolAcceptsExactlyTwentyFiveCalls(t *testing.T) {
	registry := tool.NewRegistry()
	registry.Register(&scriptedTool{id: "ok", result: &tool.Result{Output: "done"}})

	result, err := NewBatchTool(registry).Execute(
		context.Background(),
		json.RawMessage(batchCalls(repeatTool("ok", 25)...)),
	)
	require.NoError(t, err)

	assert.Equal(t, "Batch: 25/25 succeeded", result.Title)
	assert.Equal(t, 25, result.Metadata["success"])
	assert.Equal(t, 0, result.Metadata["errors"])
}

func TestBatchToolReportsPerCallOutcome(t *testing.T) {
	registry := tool.NewRegistry()
	registry.Register(&scriptedTool{id: "titled", result: &tool.Result{Title: "T", Output: "first"}})
	registry.Register(&scriptedTool{id: "untitled", result: &tool.Result{Output: "second"}})
	registry.Register(&scriptedTool{id: "broken", err: errors.New("boom")})

	result, err := NewBatchTool(registry).Execute(
		context.Background(),
		json.RawMessage(batchCalls("titled", "broken", "untitled")),
	)
	require.NoError(t, err)

	assert.Equal(t, "=== titled (call 1) ===\n[T]\nfirst\n\n"+
		"=== broken (call 2) ===\nError: boom\n\n"+
		"=== untitled (call 3) ===\nsecond", result.Output)
	assert.Equal(t, "Batch: 2/3 succeeded", result.Title)
	assert.Equal(t, 3, result.Metadata["total"])
	assert.Equal(t, 2, result.Metadata["success"])
	assert.Equal(t, 1, result.Metadata["errors"])
}

// A failing call must not stop the others, and results stay in call order.
func TestBatchToolRunsCallsConcurrently(t *testing.T) {
	release := make(chan struct{})

	first := &scriptedTool{id: "first", result: &tool.Result{Output: "1"}, release: release}
	second := &scriptedTool{id: "second", result: &tool.Result{Output: "2"}, release: release}

	registry := tool.NewRegistry()
	registry.Register(first)
	registry.Register(second)

	done := make(chan *tool.Result, 1)

	go func() {
		result, err := NewBatchTool(registry).Execute(
			context.Background(),
			json.RawMessage(batchCalls("first", "second")),
		)
		assert.NoError(t, err)
		done <- result
	}()

	assert.Eventually(t, func() bool {
		return first.started.Load() == 1 && second.started.Load() == 1
	}, 2*time.Second, 5*time.Millisecond, "both calls must start before either finishes")

	close(release)

	result := <-done
	assert.True(t, strings.HasPrefix(result.Output, "=== first (call 1) ==="))
	assert.Contains(t, result.Output, "=== second (call 2) ===")
}

func batchCalls(tools ...string) string {
	parts := make([]string, 0, len(tools))
	for _, name := range tools {
		parts = append(parts, fmt.Sprintf(`{"tool":%q,"params":{}}`, name))
	}

	return `{"calls":[` + strings.Join(parts, ",") + `]}`
}

func repeatTool(name string, n int) []string {
	names := make([]string, 0, n)
	for range n {
		names = append(names, name)
	}

	return names
}

// An agent-type allowlist is applied by filtering the registry; batch must
// follow that view instead of the full one it was constructed with.
func TestBatchToolFollowsTheRegistryItIsServedFrom(t *testing.T) {
	forbidden := &scriptedTool{id: "forbidden", result: &tool.Result{Output: "escaped"}}

	registry := tool.NewRegistry()
	registry.Register(&scriptedTool{id: "allowed", result: &tool.Result{Output: "done"}})
	registry.Register(forbidden)
	registry.Register(NewBatchTool(registry))

	filtered := registry.Filter([]string{"allowed", tool.IDBatch})

	_, err := filtered.Execute(context.Background(), tool.IDBatch, json.RawMessage(batchCalls("forbidden")))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown tool "forbidden"`)
	assert.Zero(t, forbidden.started.Load(), "a filtered-out tool must not run")

	result, err := filtered.Execute(context.Background(), tool.IDBatch, json.RawMessage(batchCalls("allowed")))
	require.NoError(t, err)
	assert.Equal(t, "Batch: 1/1 succeeded", result.Title)
}

func TestBatchToolNumbersRejectedCalls(t *testing.T) {
	registry := tool.NewRegistry()
	registry.Register(&scriptedTool{id: "ok", result: &tool.Result{Output: "done"}})
	registry.Register(NewSkillTool(loader.New()))

	_, err := NewBatchTool(registry).Execute(context.Background(), json.RawMessage(batchCalls("ok", "ok", "skill")))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "call 3: skill must be invoked directly")
}
