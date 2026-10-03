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

func assertBoundaryContinuationOrder(t *testing.T, messages []llmwire.Message, result string) {
	t.Helper()

	require.Len(t, messages, 5)
	assert.Equal(t, llmwire.RoleUser, messages[0].Role)
	assert.Equal(t, "original request", messages[0].Content)
	assert.Equal(t, llmwire.RoleAssistant, messages[1].Role)
	assert.Equal(t, llmwire.RoleTool, messages[2].Role)
	assert.Equal(t, "owned-call", messages[2].ToolCallID)
	assert.Equal(t, result, messages[2].Content)
	assert.Equal(t, llmwire.RoleUser, messages[3].Role)
	assert.Contains(t, messages[3].Content, "continue with the followup")
	assert.Equal(t, llmwire.RoleUser, messages[4].Role)
	assert.Equal(t, "active background producer snapshot", messages[4].Content)
}
