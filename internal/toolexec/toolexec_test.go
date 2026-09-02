package toolexec

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// waitFor polls cond until it holds or the deadline passes; polling keeps the
// assertions deterministic without sleeping on goroutine scheduling.
func waitFor(t *testing.T, cond func() bool) {
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

func safeCalls(ids ...string) []Call[string] {
	calls := make([]Call[string], len(ids))
	for i, id := range ids {
		calls[i] = Call[string]{Call: id, ParallelSafe: true}
	}

	return calls
}

// numberedSafeCalls returns n parallel-safe calls with ids "0".."n-1".
func numberedSafeCalls(n int) []Call[string] {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprint(i)
	}

	return safeCalls(ids...)
}

func TestPlanStages_PartitionsContiguousRuns(t *testing.T) {
	tests := []struct {
		name  string
		calls []Call[string]
		want  []stage
	}{
		{
			name:  "all safe is one stage",
			calls: safeCalls("a", "b", "c"),
			want:  []stage{{start: 0, end: 3, parallel: true}},
		},
		{
			name: "all exclusive are singleton barriers",
			calls: []Call[string]{
				{Call: "a"}, {Call: "b"}, {Call: "c"},
			},
			want: []stage{
				{start: 0, end: 1},
				{start: 1, end: 2},
				{start: 2, end: 3},
			},
		},
		{
			name: "mixed ordering splits on every barrier",
			calls: []Call[string]{
				{Call: "a", ParallelSafe: true},
				{Call: "b"},
				{Call: "c", ParallelSafe: true},
				{Call: "d", ParallelSafe: true},
			},
			want: []stage{
				{start: 0, end: 1, parallel: true},
				{start: 1, end: 2},
				{start: 2, end: 4, parallel: true},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, planStages(tt.calls))
		})
	}
}

func TestSchedule_EmptyInput(t *testing.T) {
	report := Schedule(context.Background(), []Call[string]{},
		func(context.Context, int, string) Invocation[string] { panic("not called") })

	assert.Empty(t, report.Results)
	assert.Equal(t, 0, report.Summary.Calls)
	assert.Equal(t, 0, report.Summary.Stages)
}

func TestSchedule_AllSafeRunsInOneStageAndBoundsOverlap(t *testing.T) {
	const n = 12

	var current, peak atomic.Int32

	exec := func(_ context.Context, _ int, call string) Invocation[string] {
		cur := current.Add(1)
		for {
			seen := peak.Load()
			if cur <= seen || peak.CompareAndSwap(seen, cur) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		current.Add(-1)

		return Invocation[string]{Outcome: OutcomeExecuted, Result: call}
	}

	report := Schedule(context.Background(), numberedSafeCalls(n), exec)

	require.Len(t, report.Results, n)
	for i := range report.Results {
		assert.Equal(t, OutcomeExecuted, report.Results[i].Outcome)
		assert.Equal(t, fmt.Sprint(i), report.Results[i].Result)
	}

	assert.Equal(t, 1, report.Summary.Stages)
	// The hard contract is the bound; the gated rolling test proves concurrency.
	assert.LessOrEqual(t, report.Summary.MaxParallel, MaxParallelPerStage)
	assert.Positive(t, report.Summary.MaxParallel)
	assert.Equal(t, n, report.Summary.Executed)
	assert.Equal(t, n, report.Summary.Calls)
}

// TestSchedule_RollingWindowAdmitsInInputOrder releases the initial window
// slot by slot and asserts exactly the next input index is admitted each time.
func TestSchedule_RollingWindowAdmitsInInputOrder(t *testing.T) {
	const n = 8

	releases := make([]chan struct{}, n)
	for i := range releases {
		releases[i] = make(chan struct{})
	}

	var mu sync.Mutex

	var entries []string

	exec := func(_ context.Context, _ int, call string) Invocation[string] {
		mu.Lock()
		entries = append(entries, call)
		mu.Unlock()

		if idx := call[0] - '0'; idx < MaxParallelPerStage {
			<-releases[idx]
		}

		return Invocation[string]{Outcome: OutcomeExecuted, Result: call}
	}

	done := make(chan Report[string], 1)

	go func() {
		done <- Schedule(context.Background(), safeCalls("0", "1", "2", "3", "4", "5", "6", "7"), exec)
	}()

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()

		return len(entries) == MaxParallelPerStage
	})

	// Each release frees exactly one slot, so the only call that can enter is
	// the next input index — callback entry order here is deterministic.
	for i := range n - MaxParallelPerStage {
		close(releases[i])

		next := fmt.Sprint(i + MaxParallelPerStage)
		waitFor(t, func() bool {
			mu.Lock()
			defer mu.Unlock()

			return len(entries) > i+MaxParallelPerStage && entries[i+MaxParallelPerStage] == next
		})
	}

	report := <-done

	for i := range report.Results {
		assert.Equal(t, OutcomeExecuted, report.Results[i].Outcome)
		assert.Equal(t, fmt.Sprint(i), report.Results[i].Result)
	}
}

func TestSchedule_BarrierRunsAloneBetweenStages(t *testing.T) {
	safe1Release := make(chan struct{})
	safe2Release := make(chan struct{})

	var mu sync.Mutex

	var events []string

	record := func(event string) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	}

	exec := func(_ context.Context, _ int, call string) Invocation[string] {
		record("start:" + call)

		switch call {
		case "safe1":
			<-safe1Release
		case "safe2":
			<-safe2Release
		}

		record("end:" + call)

		return Invocation[string]{Outcome: OutcomeExecuted}
	}

	calls := []Call[string]{
		{Call: "safe1", ParallelSafe: true},
		{Call: "barrier"},
		{Call: "safe2", ParallelSafe: true},
	}

	done := make(chan Report[string], 1)

	go func() {
		done <- Schedule(context.Background(), calls, exec)
	}()

	// While safe1 blocks, stage 1 cannot complete — so the barrier can be
	// neither admitted nor started. Deterministic, no snapshot race.
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()

		return contains(events, "start:safe1")
	})

	mu.Lock()
	barrierNotStarted := !contains(events, "start:barrier")
	mu.Unlock()
	require.True(t, barrierNotStarted, "barrier must not run while the first stage is in flight")

	close(safe1Release)
	close(safe2Release)

	report := <-done

	require.Len(t, report.Results, 3)
	for i := range report.Results {
		assert.Equal(t, OutcomeExecuted, report.Results[i].Outcome)
	}

	// Event order proves the barrier ran alone and between both stages.
	mu.Lock()
	defer mu.Unlock()

	assert.Less(t, indexOf(events, "end:safe1"), indexOf(events, "start:barrier"))
	assert.Less(t, indexOf(events, "start:barrier"), indexOf(events, "end:barrier"))
	assert.Less(t, indexOf(events, "end:barrier"), indexOf(events, "start:safe2"))
}

func indexOf(haystack []string, needle string) int {
	for i, s := range haystack {
		if s == needle {
			return i
		}
	}

	return -1
}

func contains(haystack []string, needle string) bool {
	return slices.Contains(haystack, needle)
}

func TestSchedule_FailStopSkipsLaterStagesButRunsWholeStage(t *testing.T) {
	exec := func(_ context.Context, _ int, call string) Invocation[string] {
		if call == "fail" {
			return Invocation[string]{Outcome: OutcomeFailed, Err: errors.New("boom"), Result: "partial"}
		}

		return Invocation[string]{Outcome: OutcomeExecuted, Result: call}
	}

	calls := []Call[string]{
		{Call: "fail", ParallelSafe: true},
		{Call: "sibling", ParallelSafe: true},
		{Call: "barrier"},
		{Call: "after", ParallelSafe: true},
	}

	report := Schedule(context.Background(), calls, exec)

	require.Len(t, report.Results, 4)

	// Whole failing stage runs: the sibling still executes next to the failure.
	assert.Equal(t, OutcomeFailed, report.Results[0].Outcome)
	assert.Equal(t, "partial", report.Results[0].Result)
	assert.Equal(t, OutcomeExecuted, report.Results[1].Outcome)

	// Later stages are explicitly skipped with the shared reason.
	for i := 2; i < 4; i++ {
		assert.Equal(t, OutcomeSkipped, report.Results[i].Outcome)
		require.ErrorIs(t, report.Results[i].Err, ErrSkipped)
	}

	assert.Equal(t, 3, report.Summary.Stages)
	assert.Equal(t, 1, report.Summary.Executed)
	assert.Equal(t, 2, report.Summary.Skipped)
	assert.Equal(t, 1, report.Summary.Failed)
}

func TestSchedule_SuspensionSkipsLaterStagesWithoutFailing(t *testing.T) {
	exec := func(_ context.Context, _ int, call string) Invocation[string] {
		if call == "suspending" {
			return Invocation[string]{Outcome: OutcomeSuspended}
		}

		return Invocation[string]{Outcome: OutcomeExecuted, Result: call}
	}

	calls := []Call[string]{
		{Call: "suspending", ParallelSafe: true},
		{Call: "sibling", ParallelSafe: true},
		{Call: "barrier"},
		{Call: "later", ParallelSafe: true},
	}

	report := Schedule(context.Background(), calls, exec)

	assert.Equal(t, OutcomeSuspended, report.Results[0].Outcome)
	assert.Equal(t, OutcomeExecuted, report.Results[1].Outcome)
	assert.Equal(t, OutcomeSkipped, report.Results[3].Outcome)
	require.ErrorIs(t, report.Results[3].Err, ErrSkipped)
	assert.Equal(t, 1, report.Summary.Suspended)
	assert.Equal(t, 0, report.Summary.Failed)
	// Both the barrier and the stage after it are skipped.
	assert.Equal(t, 2, report.Summary.Skipped)
}

func TestSchedule_CancellationStopsAdmissionKeepsStartedCallbacks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	release := make(chan struct{})

	var mu sync.Mutex

	started := 0

	exec := func(_ context.Context, _ int, call string) Invocation[string] {
		mu.Lock()
		started++
		mu.Unlock()

		if call != "barrier" {
			// The first eight hold their slots so the window cannot roll
			// before the test cancels; barrier never blocks.
			<-release
		}

		return Invocation[string]{Outcome: OutcomeExecuted, Result: call}
	}

	calls := append(numberedSafeCalls(8), Call[string]{Call: "barrier"})

	done := make(chan Report[string], 1)

	go func() {
		done <- Schedule(ctx, calls, exec)
	}()

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()

		return started == MaxParallelPerStage
	})

	cancel()

	close(release)

	report := <-done

	require.Len(t, report.Results, 9)

	for i := range MaxParallelPerStage {
		assert.Equal(t, OutcomeExecuted, report.Results[i].Outcome)
	}

	// Unadmitted calls stay cancelled, never fabricated into tool failures.
	for i := MaxParallelPerStage; i < 9; i++ {
		require.Equal(t, OutcomeCancelled, report.Results[i].Outcome)
		require.NoError(t, report.Results[i].Err)
	}

	assert.Equal(t, MaxParallelPerStage, report.Summary.Executed)
	assert.Zero(t, report.Summary.Failed)
	assert.Zero(t, report.Summary.Skipped)
}

func TestSchedule_PanicBecomesFailureAndSkipsLaterStages(t *testing.T) {
	exec := func(_ context.Context, _ int, call string) Invocation[string] {
		panic("boom " + call)
	}

	calls := []Call[string]{
		{Call: "a", ParallelSafe: true},
		{Call: "b"},
	}

	report := Schedule(context.Background(), calls, exec)

	assert.Equal(t, OutcomeFailed, report.Results[0].Outcome)
	require.ErrorContains(t, report.Results[0].Err, "boom a")
	assert.Equal(t, OutcomeSkipped, report.Results[1].Outcome)
	require.ErrorIs(t, report.Results[1].Err, ErrSkipped)
	assert.Equal(t, 1, report.Summary.Failed)
}

func TestSchedule_TypedFailureCarriesPartialResult(t *testing.T) {
	exec := func(_ context.Context, _ int, _ string) Invocation[string] {
		return Invocation[string]{Outcome: OutcomeFailed, Result: "partial output", Err: errors.New("typed")}
	}

	report := Schedule(context.Background(), safeCalls("a"), exec)

	assert.Equal(t, OutcomeFailed, report.Results[0].Outcome)
	assert.Equal(t, "partial output", report.Results[0].Result)
	assert.EqualError(t, report.Results[0].Err, "typed")
}

func TestOutcome_String(t *testing.T) {
	assert.Equal(t, "executed", OutcomeExecuted.String())
	assert.Equal(t, "failed", OutcomeFailed.String())
	assert.Equal(t, "suspended", OutcomeSuspended.String())
	assert.Equal(t, "skipped", OutcomeSkipped.String())
	assert.Equal(t, "cancelled", OutcomeCancelled.String())
	assert.Contains(t, Outcome(42).String(), "42")
}
