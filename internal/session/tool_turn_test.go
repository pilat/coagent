package session

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/tool"
)

type toolProgressEffect struct {
	change func(context.Context) (string, bool, error)
}

func (p *toolProgressEffect) ProgressChange(ctx context.Context) (string, bool, error) {
	return p.change(ctx)
}

func TestToolTurnAdoptsCommittedGrantBeforeProgressFailure(t *testing.T) {
	for _, failedCommit := range []bool{false, true} {
		t.Run(
			map[bool]string{false: "committed progress failure", true: "failed commit"}[failedCommit],
			func(t *testing.T) {
				db, store, sessionID := newFinalOutputStore(t)
				ms := newMessageStore(store, sessionID, store)
				reg := tool.NewRegistry()
				reg.Register(&imageStubTool{id: "set_budget", result: &tool.Result{
					Output: "updated", DirectMessages: []string{"Budget updated"},
				}})
				calls := []llmwire.ToolCall{{ID: "budget-call", Name: "set_budget", Arguments: []byte(`{}`)}}
				require.NoError(t, ms.addAssistantMessage(t.Context(), &llmwire.Response{ToolCalls: calls}))
				if failedCommit {
					_, err := db.ExecContext(t.Context(), `CREATE TRIGGER reject_tool_result BEFORE INSERT ON messages
 WHEN NEW.role = 'tool' BEGIN SELECT RAISE(ABORT, 'result unavailable'); END`)
					require.NoError(t, err)
				}

				progressErr := errors.New("progress unavailable")
				progressCalls := 0
				progress := &toolProgressEffect{change: func(ctx context.Context) (string, bool, error) {
					progressCalls++
					messages, err := store.LoadActiveMessages(ctx, sessionID)
					require.NoError(t, err)
					require.Len(t, messages, 2, "tool result must be durable before publishing progress")
					return "", false, progressErr
				}}
				model := newTestModelRuntime(&mockLLMClient{}, store, sessionID)
				grant := &tool.ActivationGrant{SessionID: sessionID, InputID: 7, ToolID: "set_budget"}
				agent := &svc{ms: ms, turns: newToolTurns(reg, model, ms, progress), currentActivation: grant}
				agent.Owner = newLiveCallOwner(agent)
				runner := &loopRunner{agent: agent, result: &loopResult{}, log: zap.NewNop()}
				_, err := runner.handlePreviousResult(t.Context())
				require.Error(t, err)
				var outputs int
				require.NoError(t, db.QueryRowContext(t.Context(),
					`SELECT COUNT(*) FROM session_outbox WHERE content = 'Budget updated'`).Scan(&outputs))
				if failedCommit {
					assert.Same(t, grant, agent.currentActivation)
					assert.Zero(t, progressCalls)
					assert.Zero(t, outputs)
				} else {
					require.ErrorIs(t, err, progressErr)
					assert.Nil(t, agent.currentActivation)
					assert.Equal(t, 1, progressCalls)
					assert.Equal(t, 1, outputs)
				}
			},
		)
	}
}

func executeTestToolCalls(ctx context.Context, turns toolTurns, calls []llmwire.ToolCall) error {
	_, err := turns.Execute(ctx, calls, nil, nil)
	return err
}

func testProgressBoundary(boundary InputBoundary) progressChangeBoundary {
	progress, _ := boundary.(progressChangeBoundary)
	return progress
}
