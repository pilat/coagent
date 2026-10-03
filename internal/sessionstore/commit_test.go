package sessionstore

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/transcript"
)

func TestCommit_Parts(t *testing.T) {
	for _, name := range []string{"accept and receipt", "resolutions", "linked acceptance", "messages and retry", "tool result replay", "replace", "activation", "state", "candidate CAS", "outputs and replay", "unfired", "budget suppression", "budget hidden candidate", "budget rejected attempt", "rollback", "lifecycle fence", "ownerless output", "keyless outputs", "host keyless outputs", "reset context", "final footer", "activation provenance", "activation expiry", "call pending"} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			store, db, projectID := newTestStore(t)
			attrs := map[string]any{"manager_id": "mgr"}
			if name == "ownerless output" {
				attrs = nil
			}
			root, err := store.CreateSession(ctx, projectID, "model", "", attrs)
			require.NoError(t, err)
			step := Commit{SessionID: root.ID, At: time.Now().UTC()}
			switch name {
			case "accept and receipt":
				input, enqueueErr := store.Enqueue(
					ctx,
					Input{SessionID: root.ID, Source: InputSourceUser, Content: "task"},
				)
				require.NoError(t, enqueueErr)
				step.Accept = []Accept{
					{
						InputID:    input.Input.ID,
						State:      InputStateAccepted,
						Content:    "prepared task",
						Receipt:    "skill activated",
						LinkRef:    -1,
						ModelBound: true,
					},
				}
				first, commitErr := store.Commit(ctx, step)
				require.NoError(t, commitErr)
				require.Len(t, first.MessageIDs, 1)
				require.Len(t, first.Outputs, 1)
				assertInputState(t, db, input.Input.ID, InputStateAccepted)
				replay, replayErr := store.Commit(ctx, step)
				require.NoError(t, replayErr)
				assert.Equal(t, first.MessageIDs, replay.MessageIDs)
				assert.Empty(t, replay.Outputs)
				record, readErr := store.GetSession(ctx, root.ID)
				require.NoError(t, readErr)
				assert.Equal(t, int64(1), record.ModelInputGeneration)
				return
			case "resolutions":
				for _, state := range []InputState{InputStateHandled, InputStateRejected, InputStateCancelled} {
					input, enqueueErr := store.Enqueue(
						ctx,
						Input{SessionID: root.ID, Source: InputSourceUser, Content: "command"},
					)
					require.NoError(t, enqueueErr)
					step.Accept = append(
						step.Accept,
						Accept{InputID: input.Input.ID, State: state, Reason: "resolved", LinkRef: -1},
					)
				}
			case "linked acceptance":
				input, enqueueErr := store.Enqueue(
					ctx,
					Input{SessionID: root.ID, Source: InputSourceSchedule, Content: "scheduled", DeliveryKey: "tick"},
				)
				require.NoError(t, enqueueErr)
				step.Messages = []*transcript.Message{
					{Role: "assistant", ToolCalls: commitToolCalls(t, "tick", "schedule")},
					{Role: "tool", ToolCallID: "tick", ToolName: "schedule", Content: "scheduled"},
				}
				step.Accept = []Accept{
					{InputID: input.Input.ID, State: InputStateAccepted, LinkRef: 1, ModelBound: true},
				}
			case "messages and retry":
				step.Messages = []*transcript.Message{
					{
						Role:           "assistant",
						Content:        "partial",
						CostUSD:        .25,
						FinishType:     "length",
						RejectedReason: RejectedReasonOutputLength,
						Usage:          []byte(`{"completionTokens":4}`),
					},
				}
				step.Unfired.Messages = []*transcript.Message{
					{Role: "user", Content: OutputLengthRecoveryPrompt, RetryOfRef: new(0)},
				}
			case "call pending":
				_, writeErr := store.Commit(
					ctx,
					Commit{
						SessionID: root.ID,
						Messages: []*transcript.Message{
							{Role: "assistant", ToolCalls: commitToolCalls(t, "sleep-call", "sleep")},
						},
					},
				)
				require.NoError(t, writeErr)
				store = testStore(db)
				assert.True(t, store.CallPending(ctx, root.ID, "sleep-call"))
				assert.False(t, store.CallPending(ctx, root.ID, "wrong-call"))
				_, writeErr = store.Commit(
					ctx,
					Commit{
						SessionID: root.ID,
						ToolResults: []*transcript.Message{
							{Role: "tool", ToolCallID: "sleep-call", ToolName: "sleep", Content: "awake"},
						},
					},
				)
				require.NoError(t, writeErr)
				store = testStore(db)
				assert.False(t, store.CallPending(ctx, root.ID, "sleep-call"))
				return
			case "tool result replay":
				step.ToolResults = []*transcript.Message{
					{Role: "tool", ToolCallID: "call", ToolName: "read", Content: "interrupted", ToolError: true},
				}
				first, commitErr := store.Commit(ctx, step)
				require.NoError(t, commitErr)
				replay, replayErr := store.Commit(ctx, step)
				require.NoError(t, replayErr)
				assert.Equal(t, first.MessageIDs, replay.MessageIDs)
				messages, readErr := store.LoadActiveMessages(ctx, root.ID)
				require.NoError(t, readErr)
				require.Len(t, messages, 1)
				assert.True(t, messages[0].ToolError)
				return
			case "replace":
				ids, insertErr := appendMessages(ctx, store,
					root.ID,
					[]*transcript.Message{{Role: "user", Content: "task"}, {Role: "assistant", Content: "long work"}},
				)
				require.NoError(t, insertErr)
				step.Replace = &Replace{
					HeadIDs: ids,
					Entries: []CompactionEntry{{Message: &transcript.Message{Role: "user", Content: "summary"}}},
				}
			case "activation":
				input, enqueueErr := store.Enqueue(
					ctx,
					Input{SessionID: root.ID, Source: InputSourceUser, Content: "/budget"},
				)
				require.NoError(t, enqueueErr)
				step.Accept = []Accept{
					{
						InputID:    input.Input.ID,
						State:      InputStateAccepted,
						Content:    "/budget",
						LinkRef:    -1,
						ModelBound: true,
					},
				}
				step.Activation = &ActivationChange{InputID: input.Input.ID, ToolID: "set_budget", Command: "/budget"}
				result, commitErr := store.Commit(ctx, step)
				require.NoError(t, commitErr)
				require.NotNil(t, result.Activation)
				step.Accept = nil
				step.Activation.State = ActivationConsumed
				step.Activation.ToolCallID = "budget-call"
				_, commitErr = store.Commit(ctx, step)
				require.NoError(t, commitErr)
				_, commitErr = store.Commit(ctx, step)
				require.NoError(t, commitErr)
				step.Activation.ToolCallID = "other-call"
				_, commitErr = store.Commit(ctx, step)
				require.ErrorIs(t, commitErr, ErrActivationConflict)
				step.Activation.State = ActivationExpired
				result, commitErr = store.Commit(ctx, step)
				require.NoError(t, commitErr)
				assert.Equal(t, ActivationConsumed, result.Activation.State)
				return
			case "state":
				step.State = StatePatch{
					Iteration:           new(3),
					Status:              new(SessionStatusCompleted),
					TodoItems:           new(json.RawMessage(`[{"content":"work","status":"pending"}]`)),
					ContextBaseline:     &ContextBaseline{Model: "model", PromptTokens: 200, MessageCount: 2},
					EmptyStopStreak:     new(2),
					ManagerReplyPending: new(true),
				}
			case "candidate CAS":
				step.Messages = []*transcript.Message{{Role: "assistant", Content: "candidate", FinishType: "stop"}}
				step.State.Candidate = &CandidateChange{NextRef: 0}
				result, commitErr := store.Commit(ctx, step)
				require.NoError(t, commitErr)
				step.State.Candidate = &CandidateChange{Expected: result.MessageIDs[0] + 1, NextRef: -1}
				_, commitErr = store.Commit(ctx, step)
				require.ErrorIs(t, commitErr, ErrCompletionCheckConflict)
				step.Messages = nil
				step.State.Candidate.Expected = result.MessageIDs[0]
				step.State.ConfirmedAnswerID = new(result.MessageIDs[0])
				_, commitErr = store.Commit(ctx, step)
				require.NoError(t, commitErr)
				return
			case "outputs and replay":
				step.Outputs = []Output{
					{Type: OutputMessagePersistent, Content: "answer", Key: "answer", ReleasesInput: true},
				}
				first, commitErr := store.Commit(ctx, step)
				require.NoError(t, commitErr)
				replay, replayErr := store.Commit(ctx, step)
				require.NoError(t, replayErr)
				assert.Equal(t, first.Outputs[0].OutputID, replay.Outputs[0].OutputID)
				assert.True(t, replay.Outputs[0].Existing)
				step.Outputs[0].Content = "conflict"
				_, commitErr = store.Commit(ctx, step)
				require.ErrorIs(t, commitErr, ErrOutputConflict)
				return
			case "keyless outputs", "host keyless outputs":
				for range 2 {
					if name == "keyless outputs" {
						_, err = store.Commit(
							ctx,
							Commit{
								SessionID: root.ID,
								Outputs:   []Output{{Type: OutputMessagePersistent, Content: "repeatable"}},
							},
						)
					} else {
						_, err = store.EnqueueOutput(
							ctx,
							OutputDraft{SessionID: root.ID, Type: OutputMessagePersistent, Content: "repeatable"},
						)
					}
					require.NoError(t, err)
				}
				var rows int
				require.NoError(
					t,
					db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_outbox WHERE session_id=? AND source_key IS NULL AND fingerprint IS NULL`, root.ID).
						Scan(&rows),
				)
				assert.Equal(t, 2, rows)
				return
			case "reset context":
				_, writeErr := store.Commit(
					ctx,
					Commit{
						SessionID: root.ID,
						Messages:  []*transcript.Message{{Role: "assistant", Content: "old context"}},
						State: StatePatch{
							ContextBaseline: &ContextBaseline{Model: "model", PromptTokens: 100, MessageCount: 1},
							EmptyStopStreak: new(4),
						},
					},
				)
				require.NoError(t, writeErr)
				step.State.ResetContext = true
				step.Messages = []*transcript.Message{{Role: "user", Content: "fresh opening"}}
			case "final footer":
				step.Messages = []*transcript.Message{
					{
						Role:    "assistant",
						Content: "answer",
						CostUSD: .25,
						Usage:   []byte(`{"promptTokens":10,"completionTokens":4}`),
					},
				}
				step.State.Iteration = new(3)
				step.Outputs = []Output{
					{
						Type:          OutputMessagePersistent,
						Content:       "answer",
						MessageRef:    0,
						Phase:         "final",
						FinalFooter:   &Footer{BackgroundYield: true},
						ReleasesInput: true,
					},
				}
			case "activation provenance":
				for _, source := range []InputSource{InputSourceAgent, InputSourceProcess, InputSourceSubagent, InputSourceSchedule} {
					input, enqueueErr := store.Enqueue(
						ctx,
						Input{SessionID: root.ID, Source: source, Content: "/budget"},
					)
					require.NoError(t, enqueueErr)
					_, commitErr := store.Commit(
						ctx,
						Commit{
							SessionID: root.ID,
							Accept: []Accept{
								{
									InputID:    input.Input.ID,
									State:      InputStateAccepted,
									Content:    "/budget",
									LinkRef:    -1,
									ModelBound: true,
								},
							},
							Activation: &ActivationChange{
								InputID: input.Input.ID,
								ToolID:  "set_budget",
								Command: "/budget",
							},
						},
					)
					require.ErrorIs(t, commitErr, ErrActivationConflict)
					assertInputState(t, db, input.Input.ID, InputStatePending)
				}
				return
			case "activation expiry":
				input, enqueueErr := store.Enqueue(
					ctx,
					Input{SessionID: root.ID, Source: InputSourceUser, Content: "/budget"},
				)
				require.NoError(t, enqueueErr)
				_, commitErr := store.Commit(
					ctx,
					Commit{
						SessionID: root.ID,
						Accept: []Accept{
							{
								InputID:    input.Input.ID,
								State:      InputStateAccepted,
								Content:    "/budget",
								LinkRef:    -1,
								ModelBound: true,
							},
						},
						Activation: &ActivationChange{
							InputID: input.Input.ID,
							ToolID:  "set_budget",
							Command: "/budget",
						},
					},
				)
				require.NoError(t, commitErr)
				result, commitErr := store.Commit(
					ctx,
					Commit{
						SessionID:  root.ID,
						Activation: &ActivationChange{InputID: input.Input.ID, State: ActivationExpired},
					},
				)
				require.NoError(t, commitErr)
				assert.Equal(t, ActivationExpired, result.Activation.State)
				return
			case "unfired", "budget suppression", "budget hidden candidate", "budget rejected attempt":
				step.Messages = []*transcript.Message{
					{
						Role:      "assistant",
						Content:   "checkpoint summary",
						CostUSD:   .75,
						ToolCalls: commitToolCalls(t, "danger", "bash"),
					},
				}
				step.ObserveBudget = true
				step.Unfired = Parts{
					Messages: []*transcript.Message{{Role: "user", Content: "continue"}},
					State:    StatePatch{Iteration: new(8)},
					Outputs:  []Output{{Type: OutputMessagePersistent, Content: "normal output"}},
				}
				step.State.EmptyStopStreak = new(3)
				if name == "budget hidden candidate" {
					step.Messages[0].ToolCalls = nil
					step.Unfired.State.Candidate = &CandidateChange{NextRef: 0}
				}
				if name == "budget rejected attempt" {
					step.Messages[0].FinishType = "length"
					step.Messages[0].RejectedReason = RejectedReasonOutputLength
					step.Unfired.Messages = []*transcript.Message{
						{Role: "user", Content: OutputLengthRecoveryPrompt, RetryOfRef: new(0)},
					}
				}
				if name != "unfired" {
					_, insertErr := db.ExecContext(
						ctx,
						`INSERT INTO session_budgets(root_session_id,state,generation,armed_at,baseline_cost_usd,cost_limit_usd) VALUES(?,'armed',1,?,0,.5)`,
						root.ID,
						step.At,
					)
					require.NoError(t, insertErr)
				}
			case "rollback":
				step.Messages = []*transcript.Message{{Role: "assistant", Content: "must roll back"}}
				step.State.Iteration = new(7)
				step.Outputs = []Output{
					{Type: OutputMessagePersistent, Content: "invalid ref", MessageRef: 99, Phase: "final"},
				}
			case "lifecycle fence":
				require.NoError(t, store.UpdateSessionStatus(ctx, root.ID, SessionStatusStopping))
				step.Messages = []*transcript.Message{{Role: "assistant", Content: "late answer"}}
				_, commitErr := store.Commit(ctx, step)
				require.ErrorIs(t, commitErr, ErrSessionStopping)
				step.Mode = CommitLifecycle
			case "ownerless output":
				step.Outputs = []Output{{Type: OutputMessagePersistent, Content: "no manager"}}
			}
			result, commitErr := store.Commit(ctx, step)
			if name == "rollback" {
				require.Error(t, commitErr)
				var rows, iteration int
				require.NoError(
					t,
					db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE session_id=?`, root.ID).Scan(&rows),
				)
				require.NoError(
					t,
					db.QueryRowContext(ctx, `SELECT iteration FROM sessions WHERE id=?`, root.ID).Scan(&iteration),
				)
				assert.Zero(t, rows)
				assert.Zero(t, iteration)
				return
			}
			require.NoError(t, commitErr)
			messages, readErr := store.LoadActiveMessages(ctx, root.ID)
			require.NoError(t, readErr)
			switch name {
			case "resolutions":
				assert.Empty(t, messages)
				for _, accepted := range step.Accept {
					assertInputState(t, db, accepted.InputID, accepted.State)
				}
			case "linked acceptance":
				require.Len(t, messages, 2)
				var acceptedID int64
				require.NoError(
					t,
					db.QueryRowContext(ctx, `SELECT accepted_message_id FROM session_inbox WHERE id=?`, step.Accept[0].InputID).
						Scan(&acceptedID),
				)
				assert.Equal(t, messages[1].ID, acceptedID)
			case "messages and retry":
				require.Len(t, messages, 1)
				assert.Equal(t, result.MessageIDs[0], messages[0].RetryOfMessageID)
				_, completion, cost, usageErr := store.GetSessionTreeUsage(ctx, root.ID)
				require.NoError(t, usageErr)
				assert.Equal(t, 4, completion)
				assert.InDelta(t, .25, cost, .000001)
			case "replace":
				require.Len(t, messages, 1)
				assert.Equal(t, "summary", messages[0].Content)
			case "state":
				record, getErr := store.GetSession(ctx, root.ID)
				require.NoError(t, getErr)
				assert.Equal(t, 3, record.Iteration)
				assert.Equal(t, SessionStatusCompleted, record.Status)
				assert.Equal(t, step.State.ContextBaseline, record.ContextBaseline())
				_, getErr = store.Commit(ctx, Commit{SessionID: root.ID, State: StatePatch{ClearContextBaseline: true}})
				require.NoError(t, getErr)
				record, getErr = store.GetSession(ctx, root.ID)
				require.NoError(t, getErr)
				assert.Nil(t, record.ContextBaseline())
			case "reset context":
				require.Len(t, messages, 1)
				assert.Equal(t, "fresh opening", messages[0].Content)
				record, getErr := store.GetSession(ctx, root.ID)
				require.NoError(t, getErr)
				assert.Nil(t, record.ContextBaseline())
				check, checkErr := store.LoadCompletionCheckState(ctx, root.ID)
				require.NoError(t, checkErr)
				assert.Zero(t, check.EmptyStopStreak)
			case "final footer":
				require.Len(t, result.Outputs, 1)
				assert.Contains(t, result.Outputs[0].Content, "🟣 Background\n\nanswer")
				assert.Contains(t, result.Outputs[0].Content, "💰 $0.25 total")
			case "budget suppression", "unfired", "budget hidden candidate", "budget rejected attempt":
				assert.Equal(t, name != "unfired", result.BudgetFired)
				record, getErr := store.GetSession(ctx, root.ID)
				require.NoError(t, getErr)
				check, checkErr := store.LoadCompletionCheckState(ctx, root.ID)
				require.NoError(t, checkErr)
				assert.Equal(t, 3, check.EmptyStopStreak)
				if name == "budget hidden candidate" || name == "budget rejected attempt" {
					require.Len(t, result.Outputs, 1)
					assert.Contains(t, result.Outputs[0].Content, "Budget checkpoint reached")
					assert.NotContains(t, result.Outputs[0].Content, "checkpoint summary")
					var paidRows, toolRows, retryRows int
					var cost float64
					require.NoError(
						t,
						db.QueryRowContext(ctx, `SELECT COUNT(*) FILTER(WHERE cost_usd=.75),COUNT(*) FILTER(WHERE role='tool'),COUNT(*) FILTER(WHERE retry_of_message_id IS NOT NULL),SUM(cost_usd) FROM messages WHERE session_id=?`, root.ID).
							Scan(&paidRows, &toolRows, &retryRows, &cost),
					)
					assert.Equal(t, 1, paidRows)
					assert.Zero(t, toolRows)
					assert.Zero(t, retryRows)
					assert.InDelta(t, .75, cost, .000001)
					assert.Nil(t, check.CandidateID)
					assert.False(t, store.CallPending(ctx, root.ID, "danger"))
					return
				}
				if result.BudgetFired {
					assert.Zero(t, record.Iteration)
					require.Len(t, messages, 2)
					assert.Equal(t, budgetToolNotExecuted, messages[1].Content)
					assert.False(t, store.CallPending(ctx, root.ID, "danger"))
					require.Len(t, result.Outputs, 1)
					var content string
					require.NoError(
						t,
						db.QueryRowContext(ctx, `SELECT content FROM session_outbox WHERE id=?`, result.Outputs[0].OutputID).
							Scan(&content),
					)
					assert.Contains(t, content, "checkpoint summary\n\nBudget checkpoint reached")
				} else {
					assert.Equal(t, 8, record.Iteration)
					assert.True(t, store.CallPending(ctx, root.ID, "danger"))
				}
			case "ownerless output":
				assert.Empty(t, result.Outputs)
			}
		})
	}
}

func TestEnqueue_SourcesDedupAndWaking(t *testing.T) {
	for _, source := range []InputSource{InputSourceUser, InputSourceAgent, InputSourceProcess, InputSourceSubagent, InputSourceSchedule, InputSourceCallResult} {
		for _, status := range []SessionStatus{SessionStatusActive, SessionStatusStopping, SessionStatusTerminating, SessionStatusKilled} {
			t.Run(fmt.Sprintf("%s/%s", source, status), func(t *testing.T) {
				store, _, projectID := newTestStore(t)
				root, err := store.CreateSession(
					t.Context(),
					projectID,
					"model",
					"",
					map[string]any{"manager_id": "mgr"},
				)
				require.NoError(t, err)
				require.NoError(t, store.UpdateSessionStatus(t.Context(), root.ID, status))
				input := Input{
					SessionID:   root.ID,
					Source:      source,
					Content:     "input",
					DeliveryKey: "delivery",
					Attributes:  map[string]any{"call_id": "call", "tool_id": "sleep"},
				}
				result, enqueueErr := store.Enqueue(t.Context(), input)
				producer := source == InputSourceProcess || source == InputSourceSubagent
				accepts := status == SessionStatusActive || status == SessionStatusStopping && producer
				if !accepts && !producer {
					require.ErrorIs(t, enqueueErr, ErrSessionNotAcceptingInput)
				} else {
					require.NoError(t, enqueueErr)
					assert.Equal(t, accepts, result.Applied)
				}
				if !accepts {
					assert.Empty(t, store.TakeWoken())
					return
				}
				select {
				case <-store.Woken():
				default:
					t.Fatal("committed input did not signal wake")
				}
				assert.Equal(t, []int64{root.ID}, store.TakeWoken())
				duplicate, duplicateErr := store.Enqueue(t.Context(), input)
				require.NoError(t, duplicateErr)
				assert.False(t, duplicate.Applied)
				assert.Empty(t, store.TakeWoken())
				pending, pendingErr := store.ListPending(t.Context(), root.ID)
				require.NoError(t, pendingErr)
				require.Len(t, pending, 1)
				if source == InputSourceUser {
					assert.Equal(t, "mgr", pending[0].Attributes["manager_id"])
				}
			})
		}
	}
	for _, name := range []string{"transaction commit", "transaction rollback", "unregistered transaction", "legacy schedule claim", "stopped schedule", "call result attributes", "invalid source", "stop preserves producers", "call result recovery"} {
		t.Run(name, func(t *testing.T) {
			store, db, projectID := newTestStore(t)
			root, err := store.CreateSession(t.Context(), projectID, "model", "", nil)
			require.NoError(t, err)
			input := Input{SessionID: root.ID, Source: InputSourceSchedule, Content: "tick", DeliveryKey: "tick"}
			switch name {
			case "transaction commit", "transaction rollback":
				err = store.WithTx(t.Context(), func(tx *sql.Tx) error {
					_, enqueueErr := EnqueueTx(t.Context(), tx, input)
					require.NoError(t, enqueueErr)
					assert.Empty(t, store.TakeWoken())
					select {
					case <-store.Woken():
						t.Fatal("wake before commit")
					default:
					}
					if name == "transaction rollback" {
						return fmt.Errorf("producer failed")
					}
					return nil
				})
				if name == "transaction rollback" {
					require.Error(t, err)
					assert.Empty(t, store.TakeWoken())
				} else {
					require.NoError(t, err)
					assert.Equal(t, []int64{root.ID}, store.TakeWoken())
				}
				pending, readErr := store.ListPending(t.Context(), root.ID)
				require.NoError(t, readErr)
				assert.Equal(t, name == "transaction commit", len(pending) == 1)
				return
			case "stop preserves producers":
				for _, source := range []InputSource{InputSourceUser, InputSourceAgent, InputSourceProcess, InputSourceSubagent, InputSourceSchedule, InputSourceCallResult} {
					_, enqueueErr := store.Enqueue(
						t.Context(),
						Input{
							SessionID:  root.ID,
							Source:     source,
							Content:    "input",
							Attributes: map[string]any{"call_id": "sleep", "tool_id": "sleep"},
						},
					)
					require.NoError(t, enqueueErr)
				}
				cancelled, cancelErr := store.CancelPendingInputsForStop(t.Context(), []int64{root.ID}, "stopped")
				require.NoError(t, cancelErr)
				assert.Equal(t, int64(3), cancelled)
				pending, pendingErr := store.ListPending(t.Context(), root.ID)
				require.NoError(t, pendingErr)
				require.Len(t, pending, 3)
				assert.Equal(
					t,
					[]InputSource{InputSourceProcess, InputSourceSubagent, InputSourceSchedule},
					[]InputSource{pending[0].Source, pending[1].Source, pending[2].Source},
				)
				return
			case "call result recovery":
				for _, status := range []SessionStatus{SessionStatusActive, SessionStatusSuspended, SessionStatusError, SessionStatusStopped} {
					record, createErr := store.CreateSession(t.Context(), projectID, "model", "", nil)
					require.NoError(t, createErr)
					_, enqueueErr := store.Enqueue(
						t.Context(),
						Input{
							SessionID:  record.ID,
							Source:     InputSourceCallResult,
							Content:    "awake",
							Attributes: map[string]any{"call_id": "sleep", "tool_id": "sleep"},
						},
					)
					require.NoError(t, enqueueErr)
					require.NoError(t, store.UpdateSessionStatus(t.Context(), record.ID, status))
					restarted := NewStore(db)
					runnable, readErr := restarted.ListSessionsWithRecoverableInput(t.Context())
					require.NoError(t, readErr)
					if status == SessionStatusActive || status == SessionStatusSuspended {
						assert.Contains(t, runnable, record.ID)
					} else {
						assert.NotContains(t, runnable, record.ID)
					}
				}
				return
			case "unregistered transaction":
				tx, beginErr := db.BeginTx(t.Context(), nil)
				require.NoError(t, beginErr)
				defer func() { _ = tx.Rollback() }()
				_, err = EnqueueTx(t.Context(), tx, input)
				require.ErrorContains(t, err, "requires WithTx")
				return
			case "legacy schedule claim":
				_, err = db.ExecContext(
					t.Context(),
					`INSERT INTO session_deliveries(session_id,delivery_id,kind,fingerprint,delivered_at) VALUES(?,'tick','tool_notification','legacy',?)`,
					root.ID,
					time.Now().UTC(),
				)
				require.NoError(t, err)
			case "stopped schedule":
				require.NoError(t, store.UpdateSessionStatus(t.Context(), root.ID, SessionStatusStopped))
			case "call result attributes":
				input.Source = InputSourceCallResult
			case "invalid source":
				input.Source = "invalid"
			}
			result, enqueueErr := store.Enqueue(t.Context(), input)
			if name == "call result attributes" || name == "invalid source" {
				require.Error(t, enqueueErr)
				assert.Empty(t, store.TakeWoken())
				return
			}
			require.NoError(t, enqueueErr)
			assert.Equal(t, name != "legacy schedule claim", result.Applied)
			if name == "stopped schedule" {
				record, getErr := store.GetSession(t.Context(), root.ID)
				require.NoError(t, getErr)
				assert.Equal(t, SessionStatusActive, record.Status)
			}
		})
	}
}

func commitToolCalls(t *testing.T, id, name string) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal([]llmwire.ToolCall{{ID: id, Name: name, Arguments: []byte(`{}`)}})
	require.NoError(t, err)
	return encoded
}
