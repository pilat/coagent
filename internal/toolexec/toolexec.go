package toolexec

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// MaxParallelPerStage bounds concurrent callbacks inside one parallel stage.
const MaxParallelPerStage = 4

// ErrSkipped is the reason carried by every skipped result: the session turns
// it into a model-visible error result tied to the original call ID.
var ErrSkipped = errors.New("skipped: an earlier tool call in this turn failed")

// Outcome classifies how the scheduler dispatched one call.
type Outcome uint8

const (
	// OutcomeExecuted ran and finished normally. The callback decides success
	// versus typed failure (tool.Result.IsError) and reports typed failure as
	// OutcomeFailed — the scheduler never parses result payloads.
	OutcomeExecuted Outcome = iota
	// OutcomeFailed ran to a terminal failure: Go error, recovered panic or a
	// callback-declared typed failure. Every later stage is skipped.
	OutcomeFailed
	// OutcomeSuspended left the call owned by the caller's pending-external
	// protocol. Not a failure, but later stages must not start.
	OutcomeSuspended
	// OutcomeSkipped never ran because an earlier stage failed or suspended.
	OutcomeSkipped
	// OutcomeCancelled never ran because the parent context was cancelled
	// before admission. Started callbacks are unaffected.
	OutcomeCancelled
)

func (o Outcome) String() string {
	switch o {
	case OutcomeExecuted:
		return "executed"
	case OutcomeFailed:
		return "failed"
	case OutcomeSuspended:
		return "suspended"
	case OutcomeSkipped:
		return "skipped"
	case OutcomeCancelled:
		return "cancelled"
	default:
		return fmt.Sprintf("unknown outcome %d", uint8(o))
	}
}

// Call pairs an opaque call descriptor with its declared concurrency policy.
// The resolved tool instance rides inside C so classification and execution
// cannot observe different registry states.
type Call[C any] struct {
	Call         C
	ParallelSafe bool
}

// Invocation is the callback's decided outcome for one call.
type Invocation[R any] struct {
	Outcome Outcome
	Result  R
	Err     error
}

// ExecFunc executes one call and decides its outcome. The index is the call's
// input position.
type ExecFunc[C, R any] func(ctx context.Context, index int, call C) Invocation[R]

// CallResult is one scheduled call's terminal record, positioned by Index in
// input order regardless of completion order.
type CallResult[R any] struct {
	Index   int
	Outcome Outcome
	// Result is valid only when the callback returned one (executed, or failed
	// with a typed result such as a batch's partial output).
	Result R
	Err    error
}

// Summary describes one Schedule invocation for its caller to log once.
type Summary struct {
	Calls       int
	Stages      int
	MaxParallel int
	Executed    int
	Skipped     int
	Failed      int
	Suspended   int
	DurationMS  int64
}

// Report returns every call's terminal record in input order plus the run
// summary. Cancelled calls appear only as outcomes, so Calls minus the counted
// outcomes is the cancelled remainder.
type Report[R any] struct {
	Results []CallResult[R]
	Summary Summary
}

type stage struct {
	start, end int // [start, end) into the call slice
	parallel   bool
}

// planStages partitions input order into maximal contiguous parallel-safe runs
// and singleton barriers.
func planStages[C any](calls []Call[C]) []stage {
	stages := make([]stage, 0, len(calls))

	for i := 0; i < len(calls); {
		singleton := !calls[i].ParallelSafe
		if singleton {
			stages = append(stages, stage{start: i, end: i + 1})
			i++

			continue
		}

		j := i

		for j < len(calls) && calls[j].ParallelSafe {
			j++
		}

		stages = append(stages, stage{start: i, end: j, parallel: true})
		i = j
	}

	return stages
}

// Schedule partitions calls into ordered stages and executes them: maximal
// contiguous parallel-safe runs share a rolling MaxParallelPerStage window in
// input order, every other call is a singleton barrier. A failed or suspended
// stage skips all later stages. Cancellation stops admission while started
// callbacks run to completion under the caller's ownership contract.
func Schedule[C, R any](
	ctx context.Context,
	calls []Call[C],
	exec ExecFunc[C, R],
) Report[R] {
	started := time.Now()

	report := Report[R]{Results: make([]CallResult[R], len(calls))}

	for i := range report.Results {
		report.Results[i].Index = i
	}

	report.Summary.Calls = len(calls)

	var overlap, maxOverlap atomic.Int32

	run := func(index int, call C) Invocation[R] {
		current := overlap.Add(1)

		for {
			seen := maxOverlap.Load()
			if current <= seen || maxOverlap.CompareAndSwap(seen, current) {
				break
			}
		}

		defer overlap.Add(-1)

		return invoke(ctx, exec, index, call)
	}

	blocked, cancelled := false, false

	for _, st := range planStages(calls) {
		report.Summary.Stages++

		switch {
		case cancelled:
			markRange(&report, st.start, st.end, OutcomeCancelled)

			continue
		case blocked:
			markRange(&report, st.start, st.end, OutcomeSkipped)

			for i := st.start; i < st.end; i++ {
				report.Results[i].Err = ErrSkipped
				report.Summary.Skipped++
			}

			continue
		case ctx.Err() != nil:
			cancelled = true

			markRange(&report, st.start, st.end, OutcomeCancelled)

			continue
		}

		switch {
		case st.parallel:
			cancelled = runParallelStage(ctx, &report, st, calls, run)
		default:
			inv := run(st.start, calls[st.start].Call)

			report.Results[st.start].Outcome = inv.Outcome
			report.Results[st.start].Result = inv.Result
			report.Results[st.start].Err = inv.Err
		}

		blocked = blocked || countOutcomes(&report, st.start, st.end, &report.Summary)
	}

	report.Summary.MaxParallel = int(maxOverlap.Load())
	report.Summary.DurationMS = time.Since(started).Milliseconds()

	return report
}

// runParallelStage drains one stage through a rolling window and reports
// whether the parent context cancelled the remaining admissions. Each
// goroutine writes only its own result slot.
func runParallelStage[C, R any](
	ctx context.Context,
	report *Report[R],
	st stage,
	calls []Call[C],
	run func(int, C) Invocation[R],
) bool {
	sem := make(chan struct{}, MaxParallelPerStage)

	var wg sync.WaitGroup

	cancelled := false

	for i := st.start; i < st.end; i++ {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			cancelled = true
		}

		if cancelled {
			report.Results[i].Outcome = OutcomeCancelled
			continue
		}

		wg.Add(1)

		go func(idx int, call C) {
			defer wg.Done()
			defer func() { <-sem }()

			inv := run(idx, call)

			report.Results[idx].Outcome = inv.Outcome
			report.Results[idx].Result = inv.Result
			report.Results[idx].Err = inv.Err
		}(i, calls[i].Call)
	}

	wg.Wait()

	return cancelled
}

// countOutcomes tallies one completed stage into the summary and reports
// whether the stage must block every later one.
func countOutcomes[R any](report *Report[R], start, end int, summary *Summary) bool {
	blocked := false

	for i := start; i < end; i++ {
		switch report.Results[i].Outcome {
		case OutcomeExecuted:
			summary.Executed++
		case OutcomeFailed:
			summary.Failed++
			blocked = true
		case OutcomeSuspended:
			summary.Suspended++
			blocked = true
		case OutcomeSkipped, OutcomeCancelled:
		}
	}

	return blocked
}

// invoke runs one callback with panic recovery into that call's error slot.
func invoke[C, R any](ctx context.Context, exec ExecFunc[C, R], index int, call C) Invocation[R] {
	// A panicked callback must still yield a failed invocation; recovery needs
	// an outer slot because the panicking frame cannot return a value.
	var outcome struct {
		inv   Invocation[R]
		panic any
	}

	func() {
		defer func() {
			outcome.panic = recover()
		}()

		outcome.inv = exec(ctx, index, call)
	}()

	if outcome.panic != nil {
		return Invocation[R]{
			Outcome: OutcomeFailed,
			Err:     fmt.Errorf("panic during tool call: %v", outcome.panic),
		}
	}

	return outcome.inv
}

func markRange[R any](report *Report[R], start, end int, outcome Outcome) {
	for i := start; i < end; i++ {
		report.Results[i].Outcome = outcome
	}
}
