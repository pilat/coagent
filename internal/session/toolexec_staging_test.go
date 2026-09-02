package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/tool/builtin"
	"github.com/pilat/coagent/internal/toolexec"
)

// gateTool blocks the entry-numbered call until its release channel closes and
// records the entry order, so stage tests assert scheduling without racing
// goroutines. Call index rides in arguments as {"n":"..."}.
type gateTool struct {
	id           string
	parallelSafe bool

	mu       sync.Mutex
	releases map[int]chan struct{}
	entered  []string
	errFor   map[string]error
	typedFor map[string]bool
}

func newGateTool(id string, parallelSafe bool) *gateTool {
	return &gateTool{
		id:           id,
		parallelSafe: parallelSafe,
		releases:     map[int]chan struct{}{},
		errFor:       map[string]error{},
		typedFor:     map[string]bool{},
	}
}

func (g *gateTool) ID() string                  { return g.id }
func (g *gateTool) Description() string         { return "gate" }
func (g *gateTool) Parameters() json.RawMessage { return json.RawMessage(`{}`) }
func (g *gateTool) ParallelSafe() bool          { return g.parallelSafe }

func (g *gateTool) Execute(_ context.Context, raw json.RawMessage) (*tool.Result, error) {
	var params struct {
		N string `json:"n"`
	}

	_ = json.Unmarshal(raw, &params)

	g.mu.Lock()

	g.entered = append(g.entered, params.N)
	err := g.errFor[params.N]
	release, gated := g.releases[len(g.entered)]
	g.mu.Unlock()

	if gated {
		<-release
	}

	if err != nil {
		if g.failTyped(params.N) {
			return &tool.Result{Output: err.Error(), IsError: true}, nil
		}

		return nil, err
	}

	return &tool.Result{Output: "ran:" + params.N}, nil
}

func (g *gateTool) failTyped(n string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.typedFor[n]
}

func (g *gateTool) gateAt(entryNumber int) chan struct{} {
	ch := make(chan struct{})
	g.mu.Lock()
	defer g.mu.Unlock()
	g.releases[entryNumber] = ch

	return ch
}

func (g *gateTool) fail(n string, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.errFor[n] = err
}

// failTyped makes the call return a typed tool.Result failure with the given
// payload instead of a Go error.
func (g *gateTool) setTypedFail(n string, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.errFor[n] = err
	g.typedFor[n] = true
}

func (g *gateTool) entryCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()

	return len(g.entered)
}

func gateCall(name, n string) llmwire.ToolCall {
	return llmwire.ToolCall{
		ID:        name + "-" + n,
		Name:      name,
		Arguments: json.RawMessage(`{"n":"` + n + `"}`),
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}

	t.Fatal("condition not met within deadline")
}

// The acceptance path: two parallel-safe calls share the first stage, the edit
// barrier runs alone, and the final read runs only after the barrier. A failure
// inside the first stage still lets the sibling run but skips every later stage.
func TestExecuteToolCalls_Stages(t *testing.T) {
	read := newGateTool("read", true)
	grep := newGateTool("grep", true)
	edit := newGateTool("edit", false)
	agent := newTestAgent(read, grep, edit)

	grep.fail("1", errors.New("boom"))

	require.NoError(t, executeToolCalls(t.Context(), agent, []llmwire.ToolCall{
		gateCall("read", "1"), gateCall("grep", "1"), gateCall("edit", "1"), gateCall("read", "2"),
	}))

	// The whole failing stage ran; nothing after it did.
	assert.Equal(t, 1, read.entryCount())
	assert.Equal(t, 1, grep.entryCount())
	assert.Equal(t, 0, edit.entryCount())

	messages := agent.ms.getMessages()
	require.Len(t, messages, 4)

	assert.Equal(t, "ran:1", messages[0].Content)
	assert.False(t, messages[0].ToolError)

	assert.Contains(t, messages[1].Content, "boom")
	assert.True(t, messages[1].ToolError, "the Go error persists as a typed failure row")

	assert.Equal(t, toolexec.ErrSkipped.Error(), messages[2].Content)
	assert.True(t, messages[2].ToolError)

	assert.Equal(t, toolexec.ErrSkipped.Error(), messages[3].Content)
	assert.True(t, messages[3].ToolError)
}

func TestExecuteToolCalls_StagesHappyPath(t *testing.T) {
	read := newGateTool("read", true)
	grep := newGateTool("grep", true)
	edit := newGateTool("edit", false)
	agent := newTestAgent(read, grep, edit)

	readRelease := read.gateAt(1)
	grepRelease := grep.gateAt(1)

	done := make(chan error, 1)

	go func() {
		done <- executeToolCalls(t.Context(), agent, []llmwire.ToolCall{
			gateCall("read", "1"), gateCall("grep", "1"), gateCall("edit", "1"), gateCall("read", "2"),
		})
	}()

	// Both stage-one calls are admitted before either returns.
	waitUntil(t, func() bool { return read.entryCount()+grep.entryCount() == 2 })

	// The edit barrier cannot start while stage one is in flight.
	assert.Equal(t, 0, edit.entryCount())

	close(readRelease)
	close(grepRelease)

	require.NoError(t, <-done)

	assert.Equal(t, 1, edit.entryCount())
	assert.Equal(t, 2, read.entryCount(), "the post-barrier read runs after the edit")

	messages := agent.ms.getMessages()
	require.Len(t, messages, 4)

	for i, want := range []string{"ran:1", "ran:1", "ran:1", "ran:2"} {
		assert.Equal(t, want, messages[i].Content, "results stay in call order")
		assert.False(t, messages[i].ToolError)
	}
}

// Plan decision 4: more than four foreground task calls all spawn through the
// rolling window — the per-stage bound must not starve the stage tail.
func TestExecuteToolCalls_MoreThanFourTasksAllSpawn(t *testing.T) {
	const total = 6

	task := newGateTool(tool.IDTask, true)
	agent := newTestAgent(task)

	var calls []llmwire.ToolCall

	for i := range total {
		calls = append(calls, gateCall(tool.IDTask, strconv.Itoa(i)))
	}

	// Four admitted up front block; each release admits exactly the next call.
	releases := make([]chan struct{}, 4)
	for i := range releases {
		releases[i] = task.gateAt(i + 1)
	}

	done := make(chan error, 1)

	go func() { done <- executeToolCalls(t.Context(), agent, calls) }()

	waitUntil(t, func() bool { return task.entryCount() == 4 })

	for i := range releases {
		close(releases[i])

		if next := i + 4; next < total {
			waitUntil(t, func() bool { return task.entryCount() >= next+1 })
		}
	}

	require.NoError(t, <-done)

	assert.Equal(t, total, task.entryCount())
}

// A suspended foreground task owns its call: no result row is recorded, and a
// following barrier stage never starts.
func TestExecuteToolCalls_SuspendedTaskBlocksFollowingStage(t *testing.T) {
	task := newGateTool(tool.IDTask, true)
	edit := newGateTool("edit", false)
	agent := newTestAgent(task, edit)

	task.fail("1", tool.ErrSuspend)

	require.NoError(t, executeToolCalls(t.Context(), agent, []llmwire.ToolCall{
		gateCall(tool.IDTask, "1"), gateCall("edit", "1"),
	}))

	assert.Equal(t, 0, edit.entryCount(), "no later stage starts after a suspension")
	assert.True(t, agent.suspended)

	messages := agent.ms.getMessages()
	require.Len(t, messages, 1, "the suspended call itself persists no result row")
	assert.Equal(t, llmwire.RoleTool, messages[0].Role)
	assert.Equal(
		t,
		toolexec.ErrSkipped.Error(),
		messages[0].Content,
		"the following barrier is skipped with an explicit result",
	)
	assert.True(t, messages[0].ToolError)
}

// A background-style task result is an ordinary result: the following barrier
// proceeds.
func TestExecuteToolCalls_TaskResultAllowsFollowingStage(t *testing.T) {
	task := newGateTool(tool.IDTask, true)
	edit := newGateTool("edit", false)
	agent := newTestAgent(task, edit)

	require.NoError(t, executeToolCalls(t.Context(), agent, []llmwire.ToolCall{
		gateCall(tool.IDTask, "1"), gateCall("edit", "1"),
	}))

	assert.Equal(t, 1, edit.entryCount())

	messages := agent.ms.getMessages()
	require.Len(t, messages, 2)
	assert.Equal(t, "ran:1", messages[0].Content)
	assert.False(t, messages[0].ToolError)
}

// The atomicity contract: when the store rejects the commit, a failed stage and
// its decided skips never reach the transcript separated — nothing from the turn
// survives a failed commit, and the settled set marks the skipped mutation as
// decided so no resume path can re-execute it.
func TestExecuteToolCalls_PersistenceFailureLeavesNoPartialSet(t *testing.T) {
	read := newGateTool("read", true)
	edit := newGateTool("edit", false)
	write := newGateTool("write", false)
	agent := newTestAgent(read, edit, write)

	mockStore := &mockSessionStore{insertFailAt: 1, insertErr: errors.New("disk full")}
	agent.ms = newMessageStore(mockStore, 1, nil)

	require.NoError(t, agent.ms.reloadMessages(t.Context()))

	edit.fail("1", errors.New("edit refused"))

	err := executeToolCalls(t.Context(), agent, []llmwire.ToolCall{
		gateCall("read", "1"), gateCall("edit", "1"), gateCall("write", "1"),
	})
	require.Error(t, err, "the failed commit surfaces instead of half-committing")

	for _, msg := range agent.ms.getMessages() {
		assert.NotEqual(t, llmwire.RoleTool, msg.Role, "no partial result row survives the failed commit")
	}

	// Retrying after the transient failure commits the identical set: the
	// failure row plus both skip stubs land together.
	require.NoError(t, executeToolCalls(t.Context(), agent, []llmwire.ToolCall{
		gateCall("read", "1"), gateCall("edit", "1"), gateCall("write", "1"),
	}))

	messages := agent.ms.getMessages()
	require.Len(t, messages, 3)

	assert.Contains(t, messages[1].Content, "edit refused")
	assert.True(t, messages[1].ToolError)

	assert.Equal(t, toolexec.ErrSkipped.Error(), messages[2].Content)
	assert.True(t, messages[2].ToolError, "the skipped write persists as an explicit error result")
}

// Skipped and suspended calls never enter the diversity window; only executed
// terminal outcomes do.
func TestExecuteToolCalls_LoopDetectorIgnoresSkips(t *testing.T) {
	read := newGateTool("read", true)
	grep := newGateTool("grep", true)
	edit := newGateTool("edit", false)
	agent := newTestAgent(read, grep, edit)

	grep.fail("1", errors.New("boom"))

	require.NoError(t, executeToolCalls(t.Context(), agent, []llmwire.ToolCall{
		gateCall("read", "1"), gateCall("grep", "1"), gateCall("edit", "1"),
	}))

	assert.Len(t, agent.loopDetector.window, 2, "only executed calls enter the window")

	for _, msg := range agent.ms.getMessages() {
		if msg.Role == llmwire.RoleTool {
			assert.NotContains(t, msg.Content, "LOOP WARNING", "skip stubs carry no detector warning")
		}
	}
}

// The loop-detector warning fronts only the last persisted executed/failed
// result of a turn; earlier results stay clean.
func TestExecuteToolCalls_WarningOnlyOnLastPersistedResult(t *testing.T) {
	// Parallel-safe so both failing calls share one stage and both rows persist.
	read := newGateTool("read", true)
	agent := newTestAgent(read)

	// One prior identical failure; the turn's two failures raise the streak to
	// the warn threshold, so this commit runs with actionWarnFailure.
	cause := errors.New("boom")
	persisted := fmt.Sprintf("Error: %v", fmt.Errorf("execute tool %s: %w", "read", cause))
	agent.loopDetector.record([]toolRecord{{
		name:       "read",
		resultHash: fingerprintResult(persisted),
		failed:     true,
	}})

	read.fail("1", cause)
	read.fail("2", cause)

	require.NoError(t, executeToolCalls(t.Context(), agent, []llmwire.ToolCall{
		gateCall("read", "1"), gateCall("read", "2"),
	}))

	messages := agent.ms.getMessages()
	require.Len(t, messages, 2)

	assert.NotContains(t, messages[0].Content, "LOOP WARNING",
		"earlier persisted results must not front the warning")
	assert.Contains(t, messages[1].Content, "LOOP WARNING",
		"the last persisted result fronts the warning")
}

// Each call runs the exact instance resolved while the turn was planned: a
// registry swap between planning and execution must not retarget execution.
func TestExecuteToolCalls_PlannedInstanceSurvivesRegistrySwap(t *testing.T) {
	// Non-parallel-safe so the swap lands between the two barrier stages.
	planned := newGateTool("read", false)
	agent := newTestAgent(planned)

	release := planned.gateAt(1)
	replacement := newGateTool("read", false)

	done := make(chan error, 1)

	go func() {
		done <- executeToolCalls(t.Context(), agent, []llmwire.ToolCall{
			gateCall("read", "1"), gateCall("read", "2"),
		})
	}()

	// The first call holds its barrier stage while the registry entry is swapped.
	waitUntil(t, func() bool { return planned.entryCount() == 1 })

	agent.registry.Register(replacement)

	close(release)

	require.NoError(t, <-done)

	assert.Equal(t, 2, planned.entryCount(),
		"both calls execute the instance resolved at planning time")
	assert.Zero(t, replacement.entryCount(),
		"the mid-turn registry replacement must not execute")
}

// The typed Result.IsError failure stops later stages while keeping the partial
// output and any images the failing result carried.
func TestExecuteToolCalls_TypedFailureKeepsPartialOutput(t *testing.T) {
	batch := &stubTool{id: tool.IDBatch, result: "partial batch output"}
	edit := newGateTool("edit", false)
	agent := newTestAgent(batch, edit)

	// The stub returns a normal result; mark it typed-failed the way a real
	// batch would report nested partial failure.
	batch.resultIsError = true

	require.NoError(t, executeToolCalls(t.Context(), agent, []llmwire.ToolCall{
		{ID: "batch-1", Name: tool.IDBatch, Arguments: []byte(`{}`)},
		gateCall("edit", "1"),
	}))

	assert.Equal(t, 0, edit.entryCount(), "the typed failure stops the later stage")

	messages := agent.ms.getMessages()
	require.Len(t, messages, 2)

	assert.Equal(t, "partial batch output", messages[0].Content, "partial output survives")
	assert.True(t, messages[0].ToolError, "the durable error bit is set")
	assert.NotContains(t, messages[0].Content, "Error:", "typed failures keep their payload unwrapped")

	assert.Equal(t, toolexec.ErrSkipped.Error(), messages[1].Content)
	assert.True(t, messages[1].ToolError)
}

// Native scheduling and the batch fallback must agree for equivalent calls:
// both stage-one siblings start before the barrier, a typed failure stops later
// stages, the skipped call renders the shared reason, and results stay in call
// order. Dispatch admission order is asserted, not goroutine callback entry.
func TestBatchFallbackParityWithNativeScheduling(t *testing.T) {
	const grepErr = "grep payload failure"

	buildRegistry := func(grep *gateTool) tool.Registry {
		read := newGateTool("read", true)
		edit := newGateTool("edit", false)

		reg := tool.NewRegistry()
		reg.Register(read)
		reg.Register(grep)
		reg.Register(edit)
		reg.Register(builtin.NewBatchTool(reg))

		// Stage-one siblings hold their slots until released, so both paths can
		// be observed at the same deterministic point.
		read.gateAt(1)
		grep.gateAt(2)

		return reg
	}

	runNative := func(grep *gateTool, reg tool.Registry) (*svc, chan error) {
		agent := newTestAgent()
		agent.registry = reg

		done := make(chan error, 1)

		go func() {
			done <- executeToolCalls(t.Context(), agent, []llmwire.ToolCall{
				gateCall("read", "1"), gateCall("grep", "1"), gateCall("edit", "1"),
			})
		}()

		readTool := reg.Get("read").(*gateTool)

		// Admission order: both stage-one calls hold slots; the barrier cannot
		// have started.
		waitUntil(t, func() bool { return readTool.entryCount()+grep.entryCount() == 2 })
		assert.Equal(t, 0, reg.Get("edit").(*gateTool).entryCount(), "barrier waits for stage one")

		return agent, done
	}

	grep := newGateTool("grep", true)
	grep.setTypedFail("1", errors.New(grepErr))
	nativeReg := buildRegistry(grep)
	native, nativeDone := runNative(grep, nativeReg)

	// Release stage one on the native path and let it finish.
	close(nativeReg.Get("read").(*gateTool).releases[1])
	close(grep.releases[2])
	require.NoError(t, <-nativeDone)

	// The native transcript: read ok, grep typed failure, edit skipped.
	nativeMessages := native.ms.getMessages()
	require.Len(t, nativeMessages, 3)
	assert.Equal(t, "ran:1", nativeMessages[0].Content)
	assert.Equal(t, grepErr, nativeMessages[1].Content)
	assert.True(t, nativeMessages[1].ToolError)
	assert.Equal(t, toolexec.ErrSkipped.Error(), nativeMessages[2].Content)
	assert.True(t, nativeMessages[2].ToolError)
	assert.False(t, nativeMessages[0].ToolError)

	// Fallback over the same executor with the same call list.
	fgrep := newGateTool("grep", true)
	fgrep.setTypedFail("1", errors.New(grepErr))
	fallbackReg := buildRegistry(fgrep)

	batch, ok := fallbackReg.Get(tool.IDBatch).(*builtin.BatchTool)
	require.True(t, ok)

	fdone := make(chan *tool.Result, 1)

	go func() {
		result, err := batch.Execute(t.Context(), json.RawMessage(`{"calls":[
			{"tool":"read","params":{"n":"1"}},
			{"tool":"grep","params":{"n":"1"}},
			{"tool":"edit","params":{"n":"1"}}
		]}`))
		require.NoError(t, err)
		fdone <- result
	}()

	fread := fallbackReg.Get("read").(*gateTool)

	waitUntil(t, func() bool { return fread.entryCount()+fgrep.entryCount() == 2 })
	assert.Equal(
		t,
		0,
		fallbackReg.Get("edit").(*gateTool).entryCount(),
		"barrier waits for stage one on the fallback too",
	)

	close(fread.releases[1])
	close(fgrep.releases[2])

	fallback := <-fdone

	assert.Equal(t, "Batch: 1/3 succeeded", fallback.Title)
	assert.True(t, fallback.IsError, "the typed nested failure marks the combined result")
	assert.Contains(t, fallback.Output, "=== read (call 1) ===\nran:1")
	assert.Contains(t, fallback.Output, "=== grep (call 2) ===\n"+grepErr)
	assert.Contains(t, fallback.Output, "=== edit (call 3) ===\nError: "+toolexec.ErrSkipped.Error())
	assert.Equal(t, 1, fallback.Metadata["success"])
	assert.Equal(t, 2, fallback.Metadata["errors"])
}

// One result row must fit the store's direct-output budget or the whole turn
// fails to commit; the batch fallback aggregates several children's outputs
// into one row, so capping lives on the commit path for both entry points.
func TestCapDirectOutput(t *testing.T) {
	t.Run("under budget stays intact", func(t *testing.T) {
		direct := []string{"a", "b", "c"}

		assert.Equal(t, direct, capDirectOutput(direct))
	})

	t.Run("count overflow keeps earliest and reports the rest", func(t *testing.T) {
		direct := []string{"dm-a", "dm-b", "dm-c", "dm-d", "dm-e", "dm-f"}

		capped := capDirectOutput(direct)

		require.Len(t, capped, sessionstore.MaxDirectMessages)
		assert.Equal(t, []string{"dm-a", "dm-b", "dm-c", "[direct output truncated: 3 messages omitted]"}, capped)
	})

	t.Run("empty messages are dropped without consuming budget", func(t *testing.T) {
		capped := capDirectOutput([]string{"", "dm-a", ""})

		assert.Equal(t, []string{"dm-a", "[direct output truncated: 2 messages omitted]"}, capped)
	})

	t.Run("oversized message is dropped for earlier ones", func(t *testing.T) {
		huge := strings.Repeat("x", sessionstore.MaxDirectMessageBytes+1)

		capped := capDirectOutput([]string{"a", huge, "b"})

		assert.Equal(t, []string{"a", "b", "[direct output truncated: 1 messages omitted]"}, capped)
	})

	t.Run("total byte overflow stops admission early", func(t *testing.T) {
		almost := strings.Repeat("x", sessionstore.MaxDirectTotalBytes/2)

		capped := capDirectOutput([]string{almost, almost, "tail"})

		// Two half-budget messages exactly fill the budget; the tail omits.
		assert.Equal(t, []string{almost, almost, "[direct output truncated: 1 messages omitted]"}, capped)
	})
}
