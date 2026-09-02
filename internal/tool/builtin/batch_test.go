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
	"github.com/pilat/coagent/internal/toolexec"
)

type scriptedTool struct {
	id             string
	result         *tool.Result
	err            error
	started        atomic.Int32
	release        chan struct{}
	parallelSafe   bool
	directMessages []string
}

var _ tool.Tool = (*scriptedTool)(nil)

func (s *scriptedTool) ID() string                  { return s.id }
func (s *scriptedTool) Description() string         { return s.id + " description" }
func (s *scriptedTool) Parameters() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (s *scriptedTool) ParallelSafe() bool          { return s.parallelSafe }

func (s *scriptedTool) Execute(_ context.Context, _ json.RawMessage) (*tool.Result, error) {
	s.started.Add(1)

	if s.release != nil {
		<-s.release
	}

	if s.result != nil {
		result := *s.result
		result.DirectMessages = s.directMessages

		return &result, s.err
	}

	return s.result, s.err
}

func TestBatchToolValidatesCalls(t *testing.T) {
	registry := tool.NewRegistry()
	registry.Register(&scriptedTool{id: "ok", result: &tool.Result{Output: "done"}})
	registry.Register(&scriptedTool{id: tool.IDSetProvider, result: &tool.Result{Output: "never runs"}})
	registry.Register(&activatedStubTool{scriptedTool: &scriptedTool{id: "gated"}})
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
		{
			name:    "activation-only tool",
			raw:     batchCalls("ok", "gated"),
			wantErr: "call 2: gated requires a user command turn",
		},
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

// A parallel-safe 25-call batch drains through the shared four-slot rolling
// window and still reports every call as succeeded.
func TestBatchToolAcceptsExactlyTwentyFiveCalls(t *testing.T) {
	registry := tool.NewRegistry()
	registry.Register(&scriptedTool{id: "ok", result: &tool.Result{Output: "done"}, parallelSafe: true})

	result, err := NewBatchTool(registry).Execute(
		context.Background(),
		json.RawMessage(batchCalls(repeatTool("ok", 25)...)),
	)
	require.NoError(t, err)

	assert.Equal(t, "Batch: 25/25 succeeded", result.Title)
	assert.Equal(t, 25, result.Metadata["success"])
	assert.Equal(t, 0, result.Metadata["errors"])
	assert.False(t, result.IsError)
}

// Mixed safe and exclusive calls never race: the two parallel-safe siblings
// start together while the exclusive call waits, and the exclusive call runs
// alone before the last stage.
func TestBatchToolMixedSafeAndExclusiveCallsUseBarriers(t *testing.T) {
	safeRelease := make(chan struct{})

	safeA := &scriptedTool{id: "safeA", result: &tool.Result{Output: "safeA"}, parallelSafe: true, release: safeRelease}
	safeB := &scriptedTool{id: "safeB", result: &tool.Result{Output: "safeB"}, parallelSafe: true, release: safeRelease}
	exclusive := &scriptedTool{id: "exclusive", result: &tool.Result{Output: "exclusive"}}
	safeC := &scriptedTool{id: "safeC", result: &tool.Result{Output: "safeC"}, parallelSafe: true}

	// Entry journal via started counters plus explicit ordering through the
	// exclusive tool gating on both safe calls having entered.
	registry := tool.NewRegistry()
	registry.Register(safeA)
	registry.Register(safeB)
	registry.Register(exclusive)
	registry.Register(safeC)

	done := make(chan *tool.Result, 1)

	go func() {
		result, err := NewBatchTool(registry).Execute(
			context.Background(),
			json.RawMessage(batchCalls("safeA", "safeB", "exclusive", "safeC")),
		)
		assert.NoError(t, err)
		done <- result
	}()

	assert.Eventually(t, func() bool {
		return safeA.started.Load() == 1 && safeB.started.Load() == 1
	}, 2*time.Second, 5*time.Millisecond, "the safe stage admits both siblings")

	assert.Zero(t, exclusive.started.Load(), "the exclusive barrier waits for the stage")
	assert.Zero(t, safeC.started.Load(), "the last stage waits behind the barrier")

	close(safeRelease)

	result := <-done

	assert.Equal(t, "Batch: 4/4 succeeded", result.Title)
	assert.Equal(t, int32(1), exclusive.started.Load())
	assert.Equal(t, int32(1), safeC.started.Load())
}

// Each nested call runs the exact instance resolved when the batch was planned:
// a registry swap between planning and execution must not retarget execution.
func TestBatchTool_NestedCallsRunPlannedInstances(t *testing.T) {
	registry := tool.NewRegistry()

	// Non-parallel-safe so the swap lands between the two barrier stages.
	planned := &scriptedTool{id: "read", result: &tool.Result{Output: "ran-planned"}}
	registry.Register(planned)
	registry.Register(NewBatchTool(registry))

	// The first nested call holds its barrier while the registry entry is swapped.
	planned.release = make(chan struct{})
	replacement := &scriptedTool{id: "read", result: &tool.Result{Output: "ran-replacement"}}

	done := make(chan *tool.Result, 1)

	go func() {
		result, err := NewBatchTool(registry).Execute(
			context.Background(),
			json.RawMessage(batchCalls("read", "read")),
		)
		assert.NoError(t, err)
		done <- result
	}()

	assert.Eventually(t, func() bool {
		return planned.started.Load() == 1
	}, 2*time.Second, 5*time.Millisecond, "the first nested call entered the planned instance")

	registry.Register(replacement)
	close(planned.release)

	result := <-done

	assert.Equal(t, int32(2), planned.started.Load(),
		"both nested calls execute the instance resolved at planning time")
	assert.Zero(t, replacement.started.Load(),
		"the mid-flight registry replacement must not execute")
	assert.NotContains(t, result.Output, "ran-replacement",
		"the combined result renders the planned instances only")
}

// Under the shared scheduling contract a nested Go failure is a typed failure
// that stops later stages: earlier results are kept, the failed call renders its
// error, and the skipped sibling renders the shared skip reason.
func TestBatchToolReportsPerCallOutcome(t *testing.T) {
	registry := tool.NewRegistry()
	registry.Register(&scriptedTool{id: "titled", result: &tool.Result{Title: "T", Output: "first"}})
	registry.Register(&scriptedTool{id: "broken", err: errors.New("boom")})
	registry.Register(&scriptedTool{id: "untitled", result: &tool.Result{Output: "second"}})

	result, err := NewBatchTool(registry).Execute(
		context.Background(),
		json.RawMessage(batchCalls("titled", "broken", "untitled")),
	)
	require.NoError(t, err)

	assert.Equal(t, "=== titled (call 1) ===\n[T]\nfirst\n\n"+
		"=== broken (call 2) ===\nError: execute broken: boom\n\n"+
		"=== untitled (call 3) ===\nError: "+toolexec.ErrSkipped.Error(), result.Output)
	assert.Equal(t, "Batch: 1/3 succeeded", result.Title)
	assert.Equal(t, 3, result.Metadata["total"])
	assert.Equal(t, 1, result.Metadata["success"])
	assert.Equal(t, 2, result.Metadata["errors"])
	assert.True(t, result.IsError, "any nested failure marks the combined result")
}

// Parallel-safe siblings share a stage: both must start before either finishes.
func TestBatchToolRunsCallsConcurrently(t *testing.T) {
	release := make(chan struct{})

	first := &scriptedTool{id: "first", result: &tool.Result{Output: "1"}, release: release, parallelSafe: true}
	second := &scriptedTool{id: "second", result: &tool.Result{Output: "2"}, release: release, parallelSafe: true}

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

// activatedStubTool is an activation-declaring tool for validation coverage.
type activatedStubTool struct {
	*scriptedTool
}

func (a *activatedStubTool) ActivationCommands() []string { return []string{"/gated"} }

// Direct messages from successful nested calls ride the combined result in
// nested call order.
func TestBatchToolPropagatesDirectMessages(t *testing.T) {
	registry := tool.NewRegistry()
	registry.Register(&scriptedTool{
		id:             "first",
		result:         &tool.Result{Output: "one"},
		directMessages: []string{"dm-1"},
	})
	registry.Register(&scriptedTool{
		id:             "second",
		result:         &tool.Result{Output: "two"},
		directMessages: []string{"dm-2"},
	})

	result, err := NewBatchTool(registry).Execute(
		context.Background(),
		json.RawMessage(batchCalls("first", "second")),
	)
	require.NoError(t, err)

	assert.Equal(t, []string{"dm-1", "dm-2"}, result.DirectMessages)
}
