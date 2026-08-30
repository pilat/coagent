package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

type recordingActivationBoundary struct {
	loopInputBoundary
	expired  []tool.ActivationGrant
	canceled []tool.ActivationGrant
}

func (b *recordingActivationBoundary) ExpireActivation(_ context.Context, grant tool.ActivationGrant) error {
	b.expired = append(b.expired, grant)

	return nil
}

func (b *recordingActivationBoundary) CancelActivation(_ context.Context, grant tool.ActivationGrant) error {
	b.canceled = append(b.canceled, grant)

	return nil
}

func (b *recordingActivationBoundary) grant() tool.ActivationGrant {
	return tool.ActivationGrant{SessionID: 1, InputID: 7, ToolID: "set_budget", Command: "/budget"}
}

func (b *recordingActivationBoundary) arm(agent *svc) {
	grant := b.grant()
	agent.currentActivation = &grant
}

type terminalBudgetGate struct {
	admitErr error
}

func (g *terminalBudgetGate) Admit(context.Context, time.Time) error { return g.admitErr }
func (g *terminalBudgetGate) Observe(context.Context) (bool, error)  { return false, nil }
func (g *terminalBudgetGate) PersistResponse(
	context.Context,
	*sessionstore.StoredMessage,
	string,
) (int64, bool, bool, error) {
	return 0, false, false, nil
}

func (g *terminalBudgetGate) PersistCompaction(
	context.Context,
	sessionstore.BudgetedCompaction,
) ([]int64, bool, error) {
	return nil, false, nil
}

// TestRunLoopResolvesPendingGrantOnEveryTerminalExit pins plan decision 21:
// no terminal path may leave a pending activation grant behind, because a
// pending grant blocks the durable inbox FIFO for every later user input.
func TestRunLoopResolvesPendingGrantOnEveryTerminalExit(t *testing.T) {
	grant := tool.ActivationGrant{SessionID: 1, InputID: 7, ToolID: "set_budget", Command: "/budget"}

	newBoundary := func() *recordingActivationBoundary {
		return &recordingActivationBoundary{}
	}

	tests := []struct {
		name       string
		setup      func(t *testing.T, agent *svc)
		run        func(t *testing.T, ctx context.Context, agent *svc, boundary *recordingActivationBoundary)
		wantExpire bool
		wantCancel bool
	}{
		{
			name: "llm error expires with receipt",
			setup: func(t *testing.T, agent *svc) {
				agent.llmClient = &loopScriptLLM{err: errors.New("provider down")}
			},
			run: func(t *testing.T, ctx context.Context, agent *svc, boundary *recordingActivationBoundary) {
				_, err := runLoop(ctx, agent, loopOptions{}, iterationGuard(5))
				require.Error(t, err)
			},
			wantExpire: true,
		},
		{
			name: "iteration cap expires with receipt",
			setup: func(t *testing.T, agent *svc) {
				agent.llmClient = &loopScriptLLM{responses: []*llmwire.Response{toolCallResponse("tc_1", "read")}}
				agent.maxIterations = 1
			},
			run: func(t *testing.T, ctx context.Context, agent *svc, boundary *recordingActivationBoundary) {
				_, err := runLoop(ctx, agent, loopOptions{}, iterationGuard(10))
				require.Error(t, err)
			},
			wantExpire: true,
		},
		{
			name: "empty response pause expires with receipt",
			setup: func(t *testing.T, agent *svc) {
				agent.llmClient = &loopScriptLLM{responses: []*llmwire.Response{{}}}
				agent.maxIterations = 20
			},
			run: func(t *testing.T, ctx context.Context, agent *svc, boundary *recordingActivationBoundary) {
				_, err := runLoop(ctx, agent, loopOptions{}, iterationGuard(20))
				require.NoError(t, err)
			},
			wantExpire: true,
		},
		{
			name: "budget checkpoint fire expires before park",
			setup: func(t *testing.T, agent *svc) {
				agent.llmClient = &loopScriptLLM{responses: []*llmwire.Response{textResponse("working")}}
				agent.budgetGate = &terminalBudgetGate{admitErr: ErrBudgetCheckpoint}
				agent.maxIterations = 5
			},
			run: func(t *testing.T, ctx context.Context, agent *svc, boundary *recordingActivationBoundary) {
				result, err := runLoop(ctx, agent, loopOptions{}, iterationGuard(5))
				require.NoError(t, err)
				require.True(t, result.Suspended, "a fired budget suspends the run")
			},
			wantExpire: true,
		},
		{
			name: "context cancellation cancels without receipt",
			setup: func(t *testing.T, agent *svc) {
				agent.llmClient = &loopScriptLLM{responses: []*llmwire.Response{textResponse("never")}}
			},
			run: func(t *testing.T, ctx context.Context, agent *svc, boundary *recordingActivationBoundary) {
				canceled, cancel := context.WithCancel(ctx)
				cancel()

				_, err := runLoop(canceled, agent, loopOptions{}, iterationGuard(5))
				require.ErrorIs(t, err, context.Canceled)
			},
			wantCancel: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			boundary := newBoundary()
			agent := newTestAgent(&stubTool{id: "read", result: "content"})
			agent.boundary = boundary
			agent.maxIterations = 5
			boundary.arm(agent)
			tt.setup(t, agent)

			tt.run(t, t.Context(), agent, boundary)

			assert.Nil(t, agent.currentActivation, "the svc must drop the resolved grant")
			assert.Equal(
				t, tt.wantExpire, len(boundary.expired) == 1,
				"pending activation grant was not resolved on this terminal path",
			)
			if tt.wantCancel {
				assert.Equal(t, []tool.ActivationGrant{grant}, boundary.canceled)
				assert.Empty(t, boundary.expired, "a cancelled loop context must not emit the expiry receipt")
			} else {
				assert.Equal(t, []tool.ActivationGrant{grant}, boundary.expired)
			}
		})
	}
}

// A consumed grant (tool call already issued) must survive a terminal error so
// the resume can replay the owed call instead of losing the mutation receipt.
func TestRunLoopKeepsConsumedGrantAcrossTerminalError(t *testing.T) {
	boundary := &recordingActivationBoundary{}
	agent := newTestAgent()
	agent.boundary = boundary
	agent.llmClient = &loopScriptLLM{err: errors.New("provider down")}
	agent.maxIterations = 5
	consumed := tool.ActivationGrant{
		SessionID: 1, InputID: 7, ToolID: "set_budget", Command: "/budget", ToolCallID: "tc_budget",
	}
	agent.currentActivation = &consumed

	_, err := runLoop(t.Context(), agent, loopOptions{}, iterationGuard(5))
	require.Error(t, err)

	assert.Empty(t, boundary.expired, "a consumed grant belongs to the replay contract, not expiry")
	assert.NotNil(t, agent.currentActivation, "the consumed grant stays current for the resume")
}
