package session

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

type checkpointProgressBoundary struct {
	loopInputBoundary
	toolProgressEffect
}

func TestCheckpointBudgetFireParksBeforeAnotherModelCall(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(
			map[bool]string{false: "automatic safe point", true: "explicit settled-command return"}[explicit],
			func(t *testing.T) {
				_, store, sessionID := newFinalOutputStore(t)
				ms := newMessageStore(store, sessionID, store)
				for _, message := range oversizedTranscript(32000) {
					require.NoError(t, ms.appendMessageLocked(t.Context(), &message))
				}
				armCheckpointBudget(t, store, sessionID)
				require.NoError(t, ms.reloadMessages(t.Context()))
				if explicit {
					require.NoError(t, ms.addAssistantMessage(t.Context(), textResponse("previous answer")))
				} else {
					require.NoError(t, ms.addUserMessage(t.Context(), "continue"))
				}
				client := &compactionMockLLM{contextWindow: 32000, response: &llmwire.Response{
					Text: validSummary, FinishType: llmwire.FinishStop, CostUSD: 1,
				}}
				model := newTestModelRuntime(client, store, sessionID)
				model.recordBaseline(t.Context(), 150000, len(ms.getMessages()), model.snapshot().generation)
				gate := &checkpointStoreGate{store: store.(sessionstore.BudgetCompactionStore), sessionID: sessionID}
				boundary := &checkpointProgressBoundary{}
				boundary.change = func(context.Context) (string, bool, error) {
					return "", false, errors.New("progress unavailable")
				}
				reg := tool.NewRegistry()
				prompt := newPromptBuilder(testPrompt, "")
				turns := newToolTurns(reg, model, ms, boundary)
				calls := newLiveCallOwner(&svc{ms: ms})
				owner := newCheckpointOwner(
					ms, model, prompt, turns, calls,
					store, gate, store, boundary,
					nil, nil, nil,
					checkpointOptions{id: sessionID, outputEnabled: true},
				)
				agent := &svc{
					Owner: calls, ms: ms, models: model, turns: turns, contexts: owner, budgetGate: gate,
					store: store, dispositions: store, registry: reg, prompt: prompt,
					id: sessionID, rootID: sessionID, boundary: boundary,
				}
				boundary.agent = agent
				if explicit {
					input, err := store.EnqueueInput(t.Context(), sessionID, sessionstore.InputSourceUser, "/compact")
					require.NoError(t, err)
					boundary.input = &PendingInput{ID: input.ID, Content: "/compact"}
				}
				var working []bool
				var notes []string
				result, err := runLoop(t.Context(), agent, loopOptions{
					Working: func(value bool) { working = append(working, value) },
					Notify:  func(_ context.Context, content string) error { notes = append(notes, content); return nil },
				}, iterationGuard(1))
				require.NoError(t, err)
				assert.True(t, result.Suspended)
				assert.Empty(t, result.FinalResponse)
				assert.Equal(t, 1, client.callCount, "the summary is the final provider call before parking")
				require.NotEmpty(t, working)
				assert.False(t, working[len(working)-1])
				assert.Equal(t, []string{"🔄 Compacting context...", "✅ Context compacted"}, notes)
			},
		)
	}
}

type checkpointStoreGate struct {
	terminalBudgetGate
	store     sessionstore.BudgetCompactionStore
	sessionID int64
}

func (g *checkpointStoreGate) PersistCompaction(
	ctx context.Context,
	value sessionstore.BudgetedCompaction,
) ([]int64, bool, error) {
	value.SessionID = g.sessionID
	value.RootID = g.sessionID
	result, err := g.store.ReplaceCompactedMessagesBudgeted(ctx, value)
	if err != nil {
		return nil, false, err
	}
	return result.MessageIDs, result.Fired, nil
}

func TestCheckpointProgressFailurePreservesCommittedOutcome(t *testing.T) {
	for _, tt := range []struct {
		name     string
		explicit bool
		budget   bool
	}{
		{name: "automatic success identity"},
		{name: "explicit command settles once", explicit: true},
		{name: "explicit command fires budget", explicit: true, budget: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const window = 32000
			db, store, sessionID := newFinalOutputStore(t)
			ms := newMessageStore(store, sessionID, store)
			for _, message := range oversizedTranscript(window) {
				require.NoError(t, ms.appendMessageLocked(t.Context(), &message))
			}
			var gate BudgetGate
			if tt.budget {
				armCheckpointBudget(t, store, sessionID)
				require.NoError(t, ms.reloadMessages(t.Context()))
				gate = &checkpointStoreGate{store: store.(sessionstore.BudgetCompactionStore), sessionID: sessionID}
			}

			client := &compactionMockLLM{contextWindow: window, response: &llmwire.Response{
				Text: validSummary, FinishType: llmwire.FinishStop, CostUSD: 1,
			}}
			model := newTestModelRuntime(client, store, sessionID)
			model.recordBaseline(t.Context(), 150000, len(ms.getMessages()), model.snapshot().generation)
			reg := tool.NewRegistry()
			prompt := newPromptBuilder(testPrompt, "")
			var owner contextRuntime
			progressCalls := 0
			boundary := &checkpointProgressBoundary{}
			boundary.change = func(context.Context) (string, bool, error) {
				progressCalls++
				assert.Nil(t, model.snapshot().baseline, "adopt the committed checkpoint before publication")
				control := &owner.(*checkpointOwner).control
				assert.Nil(t, control.queuedInput, "the command has left the queued state before progress publication")
				return "", false, errors.New("progress unavailable")
			}
			turns := newToolTurns(reg, model, ms, boundary)
			owner = newCheckpointOwner(
				ms, model, prompt, turns, newLiveCallOwner(&svc{ms: ms}),
				store, gate, store, boundary,
				nil, nil, nil,
				checkpointOptions{id: sessionID, outputEnabled: true},
			)
			owner.(*checkpointOwner).compactionFailures = compactionAttemptCap - 1
			var notes []string
			notify := func(_ context.Context, content string) error { notes = append(notes, content); return nil }
			var inputID int64
			if tt.explicit {
				input, err := store.EnqueueInput(t.Context(), sessionID, sessionstore.InputSourceUser, "/compact")
				require.NoError(t, err)
				inputID = input.ID
				_, err = owner.queueCommand(t.Context(), PendingInput{ID: input.ID}, "", notify)
				require.NoError(t, err)
			}

			result := owner.apply(t.Context(), notify)
			require.True(t, result.committed)
			assert.Equal(t, tt.budget, result.budgetFired)
			assert.Equal(t, []string{"🔄 Compacting context...", "✅ Context compacted"}, notes)
			assert.Equal(t, 1, progressCalls)
			assert.Nil(t, model.snapshot().baseline)
			assert.False(t, owner.(*checkpointOwner).autoCompactionOff)
			if tt.explicit {
				var state string
				require.NoError(t, db.QueryRowContext(t.Context(),
					"SELECT state FROM session_inbox WHERE id = ?", inputID).Scan(&state))
				assert.Equal(t, string(sessionstore.InputStateHandled), state)
				assert.Equal(t, compactionAttemptCap-1, owner.(*checkpointOwner).compactionFailures)
			} else {
				assert.Positive(t, owner.(*checkpointOwner).compactionSummaryDBID)
				assert.Zero(t, owner.(*checkpointOwner).compactionFailures)
			}
			record, err := store.GetSession(t.Context(), sessionID)
			require.NoError(t, err)
			assert.Zero(t, record.ContextBaselinePromptTokens)
			require.NoError(t, ms.reloadMessages(t.Context()))
			assert.True(t, hasSummaryRow(ms.getMessages()))
			owner.apply(t.Context(), notify)
			assert.Equal(t, 1, client.callCount)
			var successes, failures int
			require.NoError(t, db.QueryRowContext(
				t.Context(),
				"SELECT SUM(content = '✅ Context compacted'), SUM(content = '❌ Compaction failed') FROM session_outbox",
			).
				Scan(&successes, &failures))
			assert.Equal(t, 1, successes)
			assert.Zero(t, failures)
		})
	}
}

func armCheckpointBudget(t *testing.T, store sessionstore.Store, sessionID int64) {
	t.Helper()
	input, err := store.EnqueueInput(t.Context(), sessionID, sessionstore.InputSourceUser, "/budget")
	require.NoError(t, err)
	_, _, err = store.PromoteInputWithActivation(t.Context(), input.ID, "/budget activate",
		sessionstore.ActivationDraft{ToolID: "set_budget", Command: "/budget"})
	require.NoError(t, err)
	limit := 0.5
	_, _, err = store.ArmBudget(t.Context(), sessionstore.BudgetMutation{
		RootSessionID: sessionID, InputID: input.ID, ToolID: "set_budget", Command: "/budget",
		ToolCallID: "arm", CostLimitUSD: &limit, Receipt: "Budget armed",
	})
	require.NoError(t, err)
}
