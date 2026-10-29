package daemon

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

type terminalBudgetReleaseHook struct {
	budget.Service
	once   sync.Once
	before func(context.Context, int64, int64, string) error
}

func TestHarnessScenario_ForeignBudgetMutationPreservesCompletionAndPendingInput(t *testing.T) {
	for _, action := range []string{"arm", "clear"} {
		t.Run(action, func(t *testing.T) {
			h := newHarness(t, harnessOptions{respond: func(_ string, messages []llmwire.Message) *llmwire.Response {
				if hasUserContaining(messages, "pending follow-up") {
					return textReply("follow-up handled")
				}
				return textReply("initial work finished")
			}})
			assistant, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
			require.NoError(t, err)
			work := h.createRoot(managerAttrs(scenarioManagerID))
			initial := foreignBudgetMutation(t, h, assistant.ID, work, "initial")
			_, err = h.store.Arm(h.ctx, initial)
			require.NoError(t, err)
			mutation := foreignBudgetMutation(t, h, assistant.ID, work, action)
			h.mgr.budgets = &terminalBudgetReleaseHook{
				Service: h.mgr.budgets,
				before: func(ctx context.Context, root, generation int64, reason string) error {
					if root != work || generation != 1 || reason != "completed" {
						return fmt.Errorf(
							"unexpected terminal release: root %d generation %d reason %s",
							root,
							generation,
							reason,
						)
					}
					if err := mutateForeignBudget(ctx, h, mutation, action); err != nil {
						return err
					}
					_, err := h.store.Enqueue(ctx, sessionstore.Input{
						SessionID: work, Source: sessionstore.InputSourceUser, Content: "pending follow-up",
					})
					if err != nil {
						return fmt.Errorf("enqueue follow-up during terminal release: %w", err)
					}
					// Consume the wake while the old runner exists; no later wake may rescue teardown.
					return h.mgr.inputReady(ctx, work)
				},
			}
			collector := collectEvents(t, h.mgr.bus.SubscribeAll())
			defer collector.stop()
			require.NoError(t, h.mgr.sendToSession(h.ctx, work, "initial work"))
			collector.waitFor(
				t,
				"work idle after foreign mutation",
				func(events []controllerapi.SessionNotification) bool {
					return hasStateEvent(events, work, controllerapi.StateIdle)
				},
			)
			assert.False(t, hasSessionErrorNotice(collector.snapshot()))
			assert.Equal(t, 1, assistantSayCount(t, h, work, "initial work finished"))
			assert.Equal(t, 1, assistantSayCount(t, h, work, "follow-up handled"))
			pending, err := h.store.ListPending(h.ctx, work)
			require.NoError(t, err)
			assert.Empty(t, pending)
			record, err := h.store.Get(h.ctx, work)
			require.NoError(t, err)
			assert.Equal(t, budget.Released, record.State)
			generation, reason := int64(2), "completed"
			if action == "clear" {
				generation, reason = 1, "cleared"
			}
			assert.Equal(t, generation, record.Generation)
			assert.Equal(t, reason, record.ReleasedReason)
		})
	}
}

func (h *terminalBudgetReleaseHook) Release(
	ctx context.Context,
	root, generation int64,
	reason string,
) (*budget.Record, error) {
	var err error
	h.once.Do(func() { err = h.before(ctx, root, generation, reason) })
	if err != nil {
		return nil, err
	}

	return h.Service.Release(ctx, root, generation, reason)
}

func foreignBudgetMutation(t *testing.T, h *harness, grant, target int64, call string) budget.Mutation {
	t.Helper()
	input, err := h.store.Enqueue(h.ctx, sessionstore.Input{
		SessionID: grant, Source: sessionstore.InputSourceUser, Content: "/budget 1m",
	})
	require.NoError(t, err)
	_, err = h.store.Commit(h.ctx, sessionstore.Commit{
		SessionID: grant,
		Accept: []sessionstore.Accept{{
			InputID: input.Input.ID, State: sessionstore.InputStateAccepted, Content: "/budget 1m", LinkRef: -1,
		}},
		Activation: &sessionstore.ActivationChange{
			InputID: input.Input.ID, ToolID: budget.ToolID, Command: "/budget", State: sessionstore.ActivationPending,
		},
	})
	require.NoError(t, err)
	return budget.Mutation{
		RootSessionID: target, GrantSessionID: grant, InputID: input.Input.ID, ToolID: budget.ToolID,
		Command: "/budget", ToolCallID: call, DurationSeconds: new(int64(60)), Receipt: "Budget " + call,
	}
}

func mutateForeignBudget(ctx context.Context, h *harness, mutation budget.Mutation, action string) error {
	mutate := h.store.Arm
	if action == "clear" {
		mutation.DurationSeconds = nil
		mutate = h.store.Clear
	}

	if _, err := mutate(ctx, mutation); err != nil {
		return fmt.Errorf("apply foreign budget mutation: %w", err)
	}

	return nil
}
