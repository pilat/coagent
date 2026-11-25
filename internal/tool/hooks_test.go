package tool

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type callHooksFixture struct {
	verdict HookVerdict
}

func (f callHooksFixture) BeforeCall(context.Context, Call) HookVerdict    { return f.verdict }
func (callHooksFixture) AfterCall(context.Context, Call, *Result) []string { return nil }

func TestInvoke_RefusalSkipsExecuteAndFailsStop(t *testing.T) {
	ctx := WithCallHooks(context.Background(), callHooksFixture{verdict: HookVerdict{Refusal: "guard refused"}})
	report := runStaging(ctx, []stageInput{{Call: "first"}, {Call: "later"}},
		func(context.Context, int, string) stageOutcome {
			t.Fatal("executed refused call")
			return stageOutcome{}
		})
	require.Len(t, report.Results, 2)
	assert.Equal(t, OutcomeFailed, report.Results[0].Outcome)
	require.NotNil(t, report.Results[0].Result)
	assert.Equal(t, "guard refused", report.Results[0].Result.Output)
	assert.True(t, report.Results[0].Result.IsError)
	assert.False(t, report.Results[0].Result.Untrusted)
	assert.Equal(t, OutcomeSkipped, report.Results[1].Outcome)
}

func TestInvoke_NoHooksInContextIsUnchanged(t *testing.T) {
	report := runStaging(context.Background(), []stageInput{{Call: "call"}},
		func(context.Context, int, string) stageOutcome {
			return stageOutcome{Outcome: OutcomeExecuted, Result: "original"}
		})
	assert.Equal(t, OutcomeExecuted, report.Results[0].Outcome)
	assert.Equal(t, &Result{Output: "original"}, report.Results[0].Result)
}

func TestInvoke_HookContextReachesResult(t *testing.T) {
	ctx := WithCallHooks(
		context.Background(),
		callHooksFixture{verdict: HookVerdict{Context: []string{"operator context"}}},
	)
	report := runStaging(ctx, []stageInput{{Call: "call"}},
		func(context.Context, int, string) stageOutcome {
			return stageOutcome{Outcome: OutcomeExecuted, Result: "original"}
		})
	assert.Equal(t, []string{"operator context"}, report.Results[0].Result.HookFeedback)
}
