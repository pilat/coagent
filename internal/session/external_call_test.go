package session

import (
	"context"
	"encoding/json"
	"sync/atomic"
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

// countingTool records how many times it ran — the whole point of the staged
// mechanism is that an applied config change never runs twice.
type countingTool struct {
	id   string
	runs atomic.Int64
}

func (c *countingTool) ID() string                  { return c.id }
func (c *countingTool) Description() string         { return "counts" }
func (c *countingTool) Parameters() json.RawMessage { return json.RawMessage(`{}`) }
func (c *countingTool) ParallelSafe() bool          { return false }
func (c *countingTool) Execute(context.Context, json.RawMessage) (*tool.Result, error) {
	c.runs.Add(1)

	return &tool.Result{Output: "ran"}, nil
}

func TestHasPendingExternalCall(t *testing.T) {
	tests := []struct {
		name   string
		staged map[string]string
		msgs   []llmwire.Message
		want   bool
	}{
		{
			name:   "staged and unanswered",
			staged: map[string]string{"c1": tool.IDConfigEdit},
			msgs:   []llmwire.Message{asst("", call("c1", tool.IDConfigEdit))},
			want:   true,
		},
		{
			name:   "staged and answered",
			staged: map[string]string{"c1": tool.IDConfigEdit},
			msgs:   []llmwire.Message{asst("", call("c1", tool.IDConfigEdit)), toolRes("c1")},
			want:   false,
		},
		{
			name:   "nothing staged",
			staged: nil,
			msgs:   []llmwire.Message{asst("", call("c1", tool.IDConfigEdit))},
			want:   false,
		},
		{
			name:   "staged in a superseded turn",
			staged: map[string]string{"c1": tool.IDConfigEdit},
			msgs: []llmwire.Message{
				asst("", call("c1", tool.IDConfigEdit)),
				toolRes("c1"),
				usr("next"),
				asst("", call("c2", "read")),
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := newTestAgent()
			agent.stagedCalls = tt.staged
			setTestMessages(agent, tt.msgs)

			assert.Equal(t, tt.want, agent.HasPendingExternalCall())
		})
	}
}

// Repair protection is the wider set: an external call is protected by name even
// before the daemon has staged anything, and past a trailing user message.
func TestPendingExternalCallIDs(t *testing.T) {
	tests := []struct {
		name   string
		staged map[string]string
		msgs   []llmwire.Message
		want   []string
	}{
		{
			name: "sleep is protected, as it always was",
			msgs: []llmwire.Message{asst("", call("c1", tool.IDSleep))},
			want: []string{"c1"},
		},
		{
			name: "a config call is protected by name",
			msgs: []llmwire.Message{asst("", call("c1", tool.IDConfigEdit))},
			want: []string{"c1"},
		},
		{
			name: "an ordinary call is not",
			msgs: []llmwire.Message{asst("", call("c1", "read"))},
			want: nil,
		},
		{
			name: "protection survives a trailing user message",
			msgs: []llmwire.Message{asst("", call("c1", tool.IDConfigEdit)), usr("hurry up")},
			want: []string{"c1"},
		},
		{
			name: "mixed turn protects only the external half",
			msgs: []llmwire.Message{asst("", call("c1", tool.IDSleep), call("c2", "read"))},
			want: []string{"c1"},
		},
		{
			name: "answered calls are not protected",
			msgs: []llmwire.Message{asst("", call("c1", tool.IDSleep)), toolRes("c1")},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := newTestAgent()
			agent.stagedCalls = map[string]string{}
			for _, msg := range tt.msgs {
				for _, c := range msg.ToolCalls {
					if tool.IsExternalCall(c.Name) {
						agent.stagedCalls[c.ID] = c.Name
					}
				}
			}
			setTestMessages(agent, tt.msgs)

			got := agent.pendingExternalCallIDs()

			assert.Len(t, got, len(tt.want))

			for _, id := range tt.want {
				assert.True(t, got[id], id)
			}
		})
	}
}

// Repair stubs dangling calls so the provider never sees an unanswered tool_use.
// A pending external call is the one thing it must leave alone: the verdict needs
// that tool_use still open to land on.
func TestRepair_LeavesAPendingExternalCallAlone(t *testing.T) {
	msgs := []llmwire.Message{
		usr("replace the config"),
		asst("", call("c1", tool.IDConfigEdit)),
		usr("hurry up"),
	}

	agent := newTestAgent()
	agent.stagedCalls = map[string]string{"c1": tool.IDConfigEdit}
	setTestMessages(agent, msgs)

	repaired := repairTranscriptExcluding(msgs, agent.pendingExternalCallIDs())

	for _, m := range repaired {
		assert.NotEqual(t, "c1", m.ToolCallID, "the open tool_use must not be stubbed")
	}

	// Without the exclusion it would be stubbed — which is what makes the
	// exclusion load-bearing rather than decorative.
	stubbed := repairTranscriptExcluding(msgs, nil)
	found := false

	for _, m := range stubbed {
		if m.ToolCallID == "c1" {
			found = true
		}
	}

	assert.True(t, found)
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
