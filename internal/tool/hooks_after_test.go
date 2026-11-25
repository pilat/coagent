package tool

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type countingAfterHooks struct {
	calls int
}

type afterResultTool struct {
	Tool
	result *Result
	err    error
}

func (h *countingAfterHooks) BeforeCall(context.Context, Call) HookVerdict {
	return HookVerdict{}
}

func (h *countingAfterHooks) AfterCall(context.Context, Call, *Result) []string {
	h.calls++
	return []string{"post hook feedback"}
}

func (t *afterResultTool) Execute(context.Context, json.RawMessage) (*Result, error) {
	return t.result, t.err
}

func TestInvoke_AfterCallSkippedForFailedAndSuspendedCalls(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result *Result
		err    error
		after  int
	}{
		{name: "typed failure", result: &Result{Output: "failure", IsError: true}},
		{name: "execute error", err: errors.New("failed")},
		{name: "suspended", err: ErrSuspend},
		{name: "nil result"},
		{name: "executed", result: &Result{Output: "success"}, after: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hooks := &countingAfterHooks{}
			ctx := WithCallHooks(t.Context(), hooks)
			declared := &afterResultTool{Tool: &stagingTool{name: "fixture"}, result: tc.result, err: tc.err}
			report := Schedule(ctx, []Call{{Tool: declared, Name: declared.ID()}})
			require.Len(t, report.Results, 1)
			assert.Equal(t, tc.after, hooks.calls)
			if tc.after != 0 {
				assert.Equal(t, []string{"post hook feedback"}, report.Results[0].Result.HookFeedback)
			}
		})
	}
}
