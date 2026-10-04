package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
		{name: "producer result", toolID: tool.IDConfigEdit, result: "configuration applied", producerReplied: true},
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
		{name: "exact producer result", resultTool: tool.IDConfigEdit, resolved: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, store, id := newAttachmentsStore(t)
			transcriptRows := []llmwire.Message{
				usr("change configuration"),
				asst("", call("apply-1", tool.IDConfigEdit)),
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
			counter := &countingTool{id: tool.IDConfigEdit}
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
					ExternalCalls: map[string]string{"apply-1": tool.IDConfigEdit},
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
