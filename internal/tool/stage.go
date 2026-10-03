package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const MaxParallelPerStage = 4

const (
	OutcomeExecuted Outcome = iota
	OutcomeFailed
	OutcomeSuspended
	OutcomeSkipped
	OutcomeCancelled
)

var ErrSkipped = errors.New("skipped: an earlier tool call in this turn failed")

type (
	Outcome uint8

	// Call keeps the resolved instance fixed across planning and execution.
	// Err rejects one call at its stage without starting its side effects.
	Call struct {
		Tool      Tool
		Name      string
		ID        string
		Arguments json.RawMessage
		Err       error
	}

	CallResult struct {
		Index   int
		Outcome Outcome
		Result  *Result
		Err     error
	}

	Summary struct {
		Calls       int
		Stages      int
		MaxParallel int
		Executed    int
		Skipped     int
		Failed      int
		Suspended   int
		DurationMS  int64
	}

	Report struct {
		Results []CallResult
		Summary Summary
	}

	stage struct {
		start, end int
		parallel   bool
	}
)

// Schedule runs parallel-safe stages through a rolling four-call window.
// Failures and suspensions stop later stages; started calls always finish.
func Schedule(ctx context.Context, calls []Call) Report {
	started := time.Now()
	report := Report{Results: make([]CallResult, len(calls))}
	for i := range report.Results {
		report.Results[i].Index = i
	}
	report.Summary.Calls = len(calls)
	var overlap, maxOverlap atomic.Int32
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
		if st.parallel {
			cancelled = runParallelStage(ctx, &report, st, calls, &overlap, &maxOverlap)
		} else {
			report.Results[st.start] = invoke(ctx, calls[st.start], st.start, &overlap, &maxOverlap)
		}
		stageBlocked := countOutcomes(&report, st.start, st.end)
		blocked = blocked || stageBlocked
	}
	report.Summary.MaxParallel = int(maxOverlap.Load())
	report.Summary.DurationMS = time.Since(started).Milliseconds()
	return report
}

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

func planStages(calls []Call) []stage {
	stages := make([]stage, 0, len(calls))
	for i := 0; i < len(calls); {
		if calls[i].Tool == nil || !calls[i].Tool.ParallelSafe() {
			stages = append(stages, stage{start: i, end: i + 1})
			i++
			continue
		}
		j := i
		for j < len(calls) && calls[j].Tool != nil && calls[j].Tool.ParallelSafe() {
			j++
		}
		stages = append(stages, stage{start: i, end: j, parallel: true})
		i = j
	}
	return stages
}

func runParallelStage(
	ctx context.Context,
	report *Report,
	st stage,
	calls []Call,
	overlap, maxOverlap *atomic.Int32,
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
		go func(idx int, call Call) {
			defer wg.Done()
			defer func() { <-sem }()
			report.Results[idx] = invoke(ctx, call, idx, overlap, maxOverlap)
		}(i, calls[i])
	}
	wg.Wait()
	return cancelled
}

func countOutcomes(report *Report, start, end int) bool {
	blocked := false
	for i := start; i < end; i++ {
		switch report.Results[i].Outcome {
		case OutcomeExecuted:
			report.Summary.Executed++
		case OutcomeFailed:
			report.Summary.Failed++
			blocked = true
		case OutcomeSuspended:
			report.Summary.Suspended++
			blocked = true
		case OutcomeSkipped, OutcomeCancelled:
		}
	}
	return blocked
}

func invoke(ctx context.Context, call Call, index int, overlap, maxOverlap *atomic.Int32) (out CallResult) {
	current := overlap.Add(1)
	for {
		seen := maxOverlap.Load()
		if current <= seen || maxOverlap.CompareAndSwap(seen, current) {
			break
		}
	}
	defer overlap.Add(-1)
	out.Index = index
	defer func() {
		if recovered := recover(); recovered != nil {
			out.Outcome = OutcomeFailed
			out.Err = fmt.Errorf("panic in tool %s: %v", call.Name, recovered)
			out.Result = nil
		}
	}()
	out.Outcome = OutcomeFailed
	if call.Err != nil {
		out.Err = call.Err
		return out
	}
	if call.Tool == nil {
		out.Err = fmt.Errorf("unknown tool: %s", call.Name)
		return out
	}
	if call.ID != "" {
		ctx = WithCallID(ctx, call.ID)
	}
	result, err := call.Tool.Execute(ctx, call.Arguments)
	if err != nil {
		out.Err = fmt.Errorf("execute tool %s: %w", call.Name, err)
		if errors.Is(err, ErrSuspend) {
			out.Outcome = OutcomeSuspended
		}
		return out
	}
	if result == nil {
		out.Err = fmt.Errorf("execute tool %s: tool returned nil result", call.Name)
		return out
	}
	out.Result = result
	if !result.IsError {
		out.Outcome = OutcomeExecuted
	}
	return out
}

func markRange(report *Report, start, end int, outcome Outcome) {
	for i := start; i < end; i++ {
		report.Results[i].Outcome = outcome
	}
}
