package sessionstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/transcript"
)

func TestAcceptedResponseBudgetPrecedesEveryDisposition(t *testing.T) {
	cases := []struct {
		name   string
		kind   ResponseDispositionKind
		output string
		tools  bool
		streak int
	}{
		{name: "silent tools", kind: ResponseDispositionToolCall, tools: true},
		{name: "direct reply with tools", kind: ResponseDispositionToolCall, tools: true, output: "model text"},
		{name: "candidate", kind: ResponseDispositionCandidate},
		{name: "confirmation", kind: ResponseDispositionConfirmed, output: "model text"},
		{name: "background yield", kind: ResponseDispositionBackgroundYield, output: "model text"},
		{name: "empty background yield", kind: ResponseDispositionBackgroundYield},
		{name: "empty stop nudge", kind: ResponseDispositionEmptyStop, streak: 1},
		{
			name: "terminal empty stop", kind: ResponseDispositionEmptyStop, streak: EmptyStopTerminalStreak,
			output: EmptyStopTerminalNotice(EmptyStopTerminalStreak),
		},
		{name: "projection failure", kind: ResponseDispositionProjectionError, output: "projection failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store, db, projectID := newTestStore(t)
			id := seedCompletionSession(t, store, db, projectID)
			_, err := db.ExecContext(t.Context(), `INSERT INTO session_budgets
				(root_session_id, state, generation, armed_at, baseline_cost_usd, cost_limit_usd)
				VALUES (?, 'armed', 1, datetime('now'), 0, 0.5)`, id)
			require.NoError(t, err)
			attempt := AcceptedResponseDisposition{
				SessionID: id, RootID: id, Iteration: 1, Kind: tc.kind,
				Message: &transcript.Message{Role: assistantRole, Content: "model text", CostUSD: 0.75},
				Output:  tc.output, OutputType: OutputMessagePersistent, EmptyStopStreak: tc.streak,
			}
			if tc.tools {
				attempt.Message.ToolCalls = []byte(`[{"id":"tool-call","name":"bash","arguments":{}}]`)
			}
			if tc.kind == ResponseDispositionCandidate || (tc.kind == ResponseDispositionEmptyStop && tc.output == "") {
				attempt.Nudge = &transcript.Message{Role: userRole, Content: "host nudge"}
			}
			result, err := store.CommitAcceptedResponseDisposition(t.Context(), attempt)
			require.NoError(t, err)
			assert.True(t, result.BudgetFired)
			require.NotNil(t, result.Budget)
			assert.Equal(t, BudgetFired, result.Budget.State)
			assert.False(t, result.TerminalCommitted)
			assert.Zero(t, result.NudgeMessageID)
			require.NotNil(t, result.Output)
			var content string
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT content FROM session_outbox WHERE id = ?`,
				result.Output.OutputID).Scan(&content))
			assert.Equal(
				t,
				"Budget checkpoint reached (cost). Persisted cost: $0.750000. The limiter is no longer armed.",
				content,
			)
			messages, err := store.LoadActiveMessages(t.Context(), id)
			require.NoError(t, err)
			count := 1
			if tc.tools {
				count++
			}
			require.Len(t, messages, count)
			assert.InDelta(t, 0.75, messages[0].CostUSD, 0.000001)
			if tc.tools {
				assert.Equal(t, "tool-call", messages[1].ToolCallID)
				assert.Equal(t, budgetToolNotExecuted, messages[1].Content)
			}
			record, err := store.GetSession(t.Context(), id)
			require.NoError(t, err)
			assert.Equal(t, 1, record.Iteration)
			assert.NotEqual(t, SessionStatusError, record.Status)
		})
	}
}

func TestAcceptedResponseBudgetRollsBackAfterObservation(t *testing.T) {
	for _, failure := range []string{"tool result", "candidate fence"} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()
			store, db, projectID := newTestStore(t)
			id := seedCompletionSession(t, store, db, projectID)
			_, err := db.ExecContext(t.Context(), `INSERT INTO session_budgets
				(root_session_id, state, generation, armed_at, baseline_cost_usd, cost_limit_usd)
				VALUES (?, 'armed', 1, datetime('now'), 0, 0.5)`, id)
			require.NoError(t, err)
			attempt := AcceptedResponseDisposition{
				SessionID: id, RootID: id, Iteration: 1, Kind: ResponseDispositionToolCall,
				Message: &transcript.Message{
					Role: assistantRole, CostUSD: 0.75,
					ToolCalls: []byte(`[{"id":"tool-call","name":"bash","arguments":{}}]`),
				},
			}
			if failure == "tool result" {
				_, err = db.ExecContext(t.Context(), `CREATE TRIGGER reject_skipped_tool BEFORE INSERT ON messages
					WHEN NEW.role = 'tool' BEGIN SELECT RAISE(ABORT, 'injected tool result failure'); END`)
				require.NoError(t, err)
			} else {
				attempt.ExpectedCandidateID = 99999
			}
			_, err = store.CommitAcceptedResponseDisposition(t.Context(), attempt)
			require.Error(t, err)
			if failure == "candidate fence" {
				require.ErrorIs(t, err, ErrCompletionCheckConflict)
			}
			budget, err := store.GetBudget(t.Context(), id)
			require.NoError(t, err)
			assert.Equal(t, BudgetArmed, budget.State)
			messages, err := store.LoadActiveMessages(t.Context(), id)
			require.NoError(t, err)
			assert.Empty(t, messages)
			var outputs int
			require.NoError(
				t,
				db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM session_outbox WHERE session_id = ?`, id).
					Scan(&outputs),
			)
			assert.Zero(t, outputs)
			record, err := store.GetSession(t.Context(), id)
			require.NoError(t, err)
			assert.Zero(t, record.Iteration)
		})
	}
}
