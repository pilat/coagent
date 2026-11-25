package tool

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
)

//nolint:nonamedreturns // Recovery returns the failure written by the deferred panic handler.
func invoke(ctx context.Context, call Call, index int, overlap, maxOverlap *atomic.Int32) (out CallResult) {
	current := overlap.Add(1)

	for {
		seen := maxOverlap.Load()
		if current <= seen || maxOverlap.CompareAndSwap(seen, current) {
			break
		}
	}

	defer overlap.Add(-1)

	defer func() {
		if recovered := recover(); recovered != nil {
			out.Outcome = OutcomeFailed
			out.Err = fmt.Errorf("panic in tool %s: %v", call.Name, recovered)
			out.Result = nil
		}

		out.Index = index
	}()

	return invokeCall(ctx, call)
}

func invokeCall(ctx context.Context, call Call) CallResult {
	out := CallResult{
		Outcome: OutcomeFailed,
		Err:     call.Err,
	}
	if call.Err != nil {
		return out
	}

	if call.Tool == nil {
		out.Err = fmt.Errorf("unknown tool: %s", call.Name)
		return out
	}

	if call.ID != "" {
		ctx = WithCallID(ctx, call.ID)
	}

	hooks := callHooksFromContext(ctx)

	var verdict HookVerdict

	if hooks != nil {
		call.Projection = projectHookCall(ctx, call)

		verdict = hooks.BeforeCall(ctx, call)
		if verdict.Refusal != "" {
			out.Result = &Result{
				Output:       verdict.Refusal,
				IsError:      true,
				HookFeedback: verdict.Context,
			}

			return out
		}
	}

	out = executeCall(ctx, call)
	if hooks != nil && out.Outcome == OutcomeExecuted {
		verdict.Context = append(verdict.Context, hooks.AfterCall(ctx, call, out.Result)...)
	}

	if out.Result != nil {
		out.Result.HookFeedback = append(out.Result.HookFeedback, verdict.Context...)
	}

	return out
}

func projectHookCall(ctx context.Context, call Call) []HookCall {
	projector, ok := call.Tool.(HookProjector)
	if !ok {
		return nil
	}

	projection, err := projector.HookCalls(ctx, call.Arguments)
	if err != nil {
		return nil
	}

	return projection
}

func executeCall(ctx context.Context, call Call) CallResult {
	out := CallResult{
		Outcome: OutcomeFailed,
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
