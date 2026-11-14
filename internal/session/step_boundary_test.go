package session

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/configapply"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/sessionprompt"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/transcript"
)

func TestRun_BoundarySettlesCallsBeforeContinuation(t *testing.T) {
	for _, tc := range []struct {
		name, toolID, result string
		producerReplied      bool
	}{
		{name: "producer result", toolID: tool.IDTask, result: "configuration applied", producerReplied: true},
		{name: "interrupted sleep", toolID: tool.IDSleep, result: sleepInterruptedMessage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, store, id := newAttachmentsStore(t)
			ctx := t.Context()
			assistant := asst("", call("owned-call", tc.toolID))
			stored, err := storedMessage(&assistant)
			require.NoError(t, err)
			_, err = store.Commit(ctx, sessionstore.Commit{
				SessionID: id,
				Messages: []*transcript.Message{
					{Role: llmwire.RoleUser, Content: "original request"}, stored,
				},
			})
			require.NoError(t, err)

			if tc.producerReplied {
				_, err = store.Enqueue(ctx, sessionstore.Input{
					SessionID: id, Source: sessionstore.InputSourceCallResult, Content: tc.result,
					Attributes: map[string]any{"call_id": "owned-call", "tool_id": tc.toolID},
				})
				require.NoError(t, err)
			}

			followup, err := store.Enqueue(ctx, sessionstore.Input{
				SessionID: id, Source: sessionstore.InputSourceUser, Content: "continue with the followup",
			})
			require.NoError(t, err)
			record, err := store.GetSession(ctx, id)
			require.NoError(t, err)
			counter := &countingTool{id: tc.toolID}
			registry := tool.NewRegistry()
			registry.Register(counter)
			prompt := sessionprompt.NewBuilder("stable", "")
			prompt.Todos = todo.New()
			client := &loopScriptLLM{onCall: func(n int, messages []llmwire.Message) (*llmwire.Response, error) {
				if n == 1 {
					assertBoundaryContinuationOrder(t, messages, tc.result)
					rows, loadErr := store.LoadActiveMessages(ctx, id)
					require.NoError(t, loadErr)
					require.Len(t, rows, 5, "provider sees the committed boundary before answering")
					var durable []llmwire.Message
					for _, row := range rows {
						durable = append(durable, llmwire.Message{
							Role: row.Role, Content: row.Content, ToolCallID: row.ToolCallID,
						})
					}
					assertBoundaryContinuationOrder(t, durable, tc.result)
				}

				return textResponse("done"), nil
			}}
			s, err := New(ctx, Input{
				Record: record, Client: client, Loader: loader.New(), Registry: registry,
				Prompt: prompt, Store: store, OutputEnabled: true,
				ExternalCalls:      map[string]string{"owned-call": tc.toolID},
				BackgroundSnapshot: "active background producer snapshot",
			})
			require.NoError(t, err)
			t.Cleanup(s.Close)
			result, err := s.Run(ctx)
			require.NoError(t, err)
			assert.Equal(t, "done", result.Final)
			assert.Equal(t, 2, client.calls, "boundary settlement needs no intervening model turn")
			assert.Zero(t, counter.runs.Load(), "the persisted call never executes again")
			pending, err := store.ListPending(ctx, id)
			require.NoError(t, err)
			assert.Empty(t, pending)
			var results, accepted int
			require.NoError(t, db.QueryRowContext(ctx,
				`SELECT count(*) FROM messages WHERE session_id = ? AND role = 'tool' AND tool_call_id = ?`,
				id, "owned-call").Scan(&results))
			assert.Equal(t, 1, results)
			require.NoError(t, db.QueryRowContext(ctx,
				`SELECT count(*) FROM session_inbox WHERE id = ? AND state = 'accepted'`,
				followup.Input.ID).Scan(&accepted))
			assert.Equal(t, 1, accepted)
		})
	}
}

func TestRun_ExternalCallOwnership(t *testing.T) {
	for _, tc := range []struct {
		name, resultTool string
		resolved         bool
	}{
		{name: "producer has not replied"},
		{name: "wrong producer result", resultTool: tool.IDSleep},
		{name: "exact producer result", resultTool: tool.IDTask, resolved: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, store, id := newAttachmentsStore(t)
			transcriptRows := []llmwire.Message{
				usr("change configuration"),
				asst("", call("apply-1", tool.IDTask)),
				asst("", call("newer-event", "process_event")),
				{
					Role:       llmwire.RoleTool,
					ToolCallID: "newer-event",
					ToolName:   "process_event",
					Content:    "process finished",
				},
			}
			var rows []*transcript.Message
			for i := range transcriptRows {
				row, err := storedMessage(&transcriptRows[i])
				require.NoError(t, err)
				rows = append(rows, row)
			}
			_, err := store.Commit(t.Context(), sessionstore.Commit{SessionID: id, Messages: rows})
			require.NoError(t, err)
			if tc.resultTool != "" {
				_, err = store.Enqueue(
					t.Context(),
					sessionstore.Input{
						SessionID:  id,
						Source:     sessionstore.InputSourceCallResult,
						Content:    "applied",
						Attributes: map[string]any{"call_id": "apply-1", "tool_id": tc.resultTool},
					},
				)
				require.NoError(t, err)
			}
			record, err := store.GetSession(t.Context(), id)
			require.NoError(t, err)
			counter := &countingTool{id: tool.IDTask}
			reg := tool.NewRegistry()
			reg.Register(counter)
			prompt := sessionprompt.NewBuilder("stable", "")
			prompt.Todos = todo.New()
			client := &loopScriptLLM{responses: []*llmwire.Response{textResponse("done"), textResponse("confirmed")}}
			s, err := New(
				t.Context(),
				Input{
					Record:        record,
					Client:        client,
					Loader:        loader.New(),
					Registry:      reg,
					Prompt:        prompt,
					Store:         store,
					ExternalCalls: map[string]string{"apply-1": tool.IDTask},
					OutputEnabled: true,
				},
			)
			require.NoError(t, err)
			t.Cleanup(s.Close)
			result, err := s.Run(t.Context())
			require.NoError(t, err)
			assert.Zero(t, counter.runs.Load(), "an owned call never executes again")
			if tc.resolved {
				assert.False(t, result.Suspended)
				assert.Equal(t, "done", result.Final)
				assert.Equal(t, 2, client.calls)
			} else {
				assert.True(t, result.Suspended)
				assert.Zero(t, client.calls)
			}
			pending, err := store.ListPending(t.Context(), id)
			require.NoError(t, err)
			assert.Empty(t, pending)
			messages := s.ms.getMessages()
			var exactResults int
			for _, row := range messages {
				if row.Role == llmwire.RoleTool && row.ToolCallID == "apply-1" {
					exactResults++
					assert.Equal(t, "applied", row.Content)
				}
			}
			assert.Equal(t, map[bool]int{false: 0, true: 1}[tc.resolved], exactResults)
		})
	}
}

// "Call it as the only tool" made a model skip reading the state it had to
// preserve; preparatory calls in earlier responses must stay allowed.
func TestActivationInstruction_AllowsPreparationButIsolatesTheMutation(t *testing.T) {
	t.Parallel()

	text := activationInstruction(tool.IDConfigEdit, "/config")

	assert.Contains(t, text, "authorizes config_edit to handle /config")
	assert.Contains(t, text, "Earlier responses in this turn may use other tools to gather what config_edit needs")
	assert.Contains(t, text, "the response that calls config_edit must contain no other tool call")
	assert.Contains(t, text, "No change has occurred until the host emits its receipt")
	assert.NotContains(t, text, "%!")
}

func TestBoundaryActivationRequiresHumanInput(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source sessionstore.InputSource
		owner  string
		human  bool
	}{
		{"user", sessionstore.InputSourceUser, "owner", true},
		{"agent", sessionstore.InputSourceAgent, "owner", false},
		{"schedule", sessionstore.InputSourceSchedule, "owner", false},
		{"process", sessionstore.InputSourceProcess, "owner", false},
		{"subagent", sessionstore.InputSourceSubagent, "owner", false},
		{"ownerless user", sessionstore.InputSourceUser, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db, store, s := newBoundaryFixture(t, nil)
			if tc.owner == "" {
				_, err := db.ExecContext(t.Context(), "UPDATE sessions SET attributes = '{}' WHERE id = ?", s.id)
				require.NoError(t, err)
			}
			s.registry.Register(configapply.NewConfigEdit(s.id, nil))
			require.NoError(t, s.prepareRun(t.Context()))
			input, err := store.Enqueue(t.Context(), sessionstore.Input{
				SessionID:  s.id,
				Source:     tc.source,
				Content:    "/config x",
				Attributes: map[string]any{"manager_id": tc.owner},
			})
			require.NoError(t, err)
			batch := s.newBoundaryBatch()
			_, err = s.acceptBoundaryText(t.Context(), &runState{}, batch, input.Input, "/config")
			require.NoError(t, err)
			require.Len(t, batch.commit.Messages, 1)
			content := batch.commit.Messages[0].Content
			assert.Contains(t, content, "/config x")
			assert.Nil(t, s.currentActivation, "authority starts only after acceptance commits")
			_, err = s.commitBoundaryBatch(t.Context(), &runState{}, batch, 1)
			require.NoError(t, err)
			if tc.human {
				assert.Contains(t, content, "[Host activation")
				assert.Equal(t, &tool.ActivationGrant{
					SessionID: s.id, InputID: input.Input.ID, ToolID: tool.IDConfigEdit, Command: "/config",
				}, s.currentActivation)
			} else {
				assert.NotContains(t, content, "[Host activation")
				assert.Nil(t, s.currentActivation)
			}
		})
	}
}

func TestBoundaryActivationSurvivesAStaleCallResult(t *testing.T) {
	_, store, s := newBoundaryFixture(t, []llmwire.Message{
		usr("earlier task"), asst("", call("old-task", tool.IDTask)), toolRes("old-task"), asst("done"),
	})
	s.registry.Register(configapply.NewConfigEdit(s.id, nil))
	require.NoError(t, s.prepareRun(t.Context()))
	_, err := store.Enqueue(t.Context(), sessionstore.Input{
		SessionID: s.id, Source: sessionstore.InputSourceUser, Content: "/config x",
	})
	require.NoError(t, err)
	_, err = s.boundaryStep(t.Context(), &runState{})
	require.NoError(t, err)
	grant := s.currentActivation
	require.NotNil(t, grant)
	_, err = store.Enqueue(t.Context(), sessionstore.Input{
		SessionID: s.id, Source: sessionstore.InputSourceCallResult, Content: "late child result",
		Attributes: map[string]any{"call_id": "old-task", "tool_id": tool.IDTask},
	})
	require.NoError(t, err)
	_, err = store.Enqueue(t.Context(), sessionstore.Input{
		SessionID: s.id, Source: sessionstore.InputSourceUser, Content: "wait for the command",
	})
	require.NoError(t, err)
	_, err = s.boundaryStep(t.Context(), &runState{})
	require.NoError(t, err)
	assert.Equal(t, grant, s.currentActivation)
	pending, err := store.ListPending(t.Context(), s.id)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, "wait for the command", pending[0].RawContent)
}

func TestRunBoundaryScopesHumanInput(t *testing.T) {
	db, store, s := newBoundaryFixture(t, []llmwire.Message{usr("history"), asst("answer")})
	r := &runState{}
	human, err := store.Enqueue(t.Context(), sessionstore.Input{
		SessionID: s.id, Source: sessionstore.InputSourceUser, Content: "current",
	})
	require.NoError(t, err)
	accepted, err := s.boundaryStep(t.Context(), r)
	require.NoError(t, err)
	require.True(t, accepted)
	var humanMessage int64
	require.NoError(t, db.QueryRowContext(t.Context(),
		`SELECT accepted_message_id FROM session_inbox WHERE id = ?`, human.Input.ID).Scan(&humanMessage))
	require.Positive(t, humanMessage)
	event, err := store.Enqueue(t.Context(), sessionstore.Input{
		SessionID: s.id, Source: sessionstore.InputSourceAgent, Content: "mid-run event",
	})
	require.NoError(t, err)
	accepted, err = s.boundaryStep(t.Context(), r)
	require.NoError(t, err)
	require.True(t, accepted)
	var eventMessage int64
	require.NoError(t, db.QueryRowContext(t.Context(),
		`SELECT accepted_message_id FROM session_inbox WHERE id = ?`, event.Input.ID).Scan(&eventMessage))
	assert.Greater(t, eventMessage, humanMessage)
	accepted, err = s.boundaryStep(t.Context(), &runState{})
	require.NoError(t, err)
	assert.False(t, accepted)
	rows, err := store.LoadActiveMessages(t.Context(), s.id)
	require.NoError(t, err)
	require.Len(t, rows, 4)
	assert.Equal(t, humanMessage, rows[2].ID)
	assert.Contains(t, rows[2].Content, "current")
	assert.Equal(t, eventMessage, rows[3].ID)
	assert.Contains(t, rows[3].Content, "mid-run event")
}

func newBoundaryFixture(t *testing.T, previous []llmwire.Message) (*sql.DB, *sessionstore.Store, *Session) {
	t.Helper()
	db, store, id := newAttachmentsStore(t)
	var rows []*transcript.Message
	for i := range previous {
		row, err := storedMessage(&previous[i])
		require.NoError(t, err)
		rows = append(rows, row)
	}
	_, err := store.Commit(t.Context(), sessionstore.Commit{SessionID: id, Messages: rows})
	require.NoError(t, err)
	record, err := store.GetSession(t.Context(), id)
	require.NoError(t, err)
	prompt := sessionprompt.NewBuilder("stable", "")
	prompt.Todos = todo.New()
	s, err := New(t.Context(), Input{
		Record:        record,
		Client:        &loopScriptLLM{responses: []*llmwire.Response{textResponse("done"), textResponse("confirmed")}},
		Loader:        loader.New(),
		Registry:      tool.NewRegistry(),
		Prompt:        prompt,
		Store:         store,
		OutputEnabled: true,
	})
	require.NoError(t, err)
	t.Cleanup(s.Close)
	return db, store, s
}
