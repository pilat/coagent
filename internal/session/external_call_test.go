package session

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/tool"
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

func stagedRunner(agent *svc) *loopRunner {
	return &loopRunner{agent: agent, result: &loopResult{}, log: zap.NewNop()}
}

// A staged call is one the daemon is already answering. Waking the session — by
// a user message, or by any other notification — must not run it a second time.
func TestHandlePreviousResult_StagedCallIsNeverReExecuted(t *testing.T) {
	tests := []struct {
		name string
		msgs []llmwire.Message
	}{
		{
			name: "suspended on the call",
			msgs: []llmwire.Message{usr("add the provider"), asst("", call("c1", tool.IDSetProvider))},
		},
		{
			name: "a user message raced the verdict",
			msgs: []llmwire.Message{
				usr("add the provider"),
				asst("", call("c1", tool.IDSetProvider)),
				usr("actually, hurry up"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			counter := &countingTool{id: tool.IDSetProvider}
			agent := newTestAgent(counter)
			agent.stagedCalls = map[string]string{"c1": tool.IDSetProvider}
			agent.ms.setMessages(tt.msgs)

			r := stagedRunner(agent)

			done, err := r.handlePreviousResult(context.Background())
			require.NoError(t, err)
			assert.True(t, done, "the loop stops until the verdict lands")
			assert.True(t, r.result.Suspended)
			assert.Equal(t, int64(0), counter.runs.Load(), "a staged call must never be executed again")
		})
	}
}

// Regression for session 125: a synthetic completion pair appended after a
// sleep must not make the older sleep disappear from causal state. The ledger,
// not the latest assistant turn, owns the unresolved call.
func TestHandlePreviousResult_StagedCallSurvivesNewerSyntheticPair(t *testing.T) {
	counter := &countingTool{id: tool.IDSleep}
	agent := newTestAgent(counter)
	agent.stagedCalls = map[string]string{"sleep-call-125": tool.IDSleep}
	agent.ms.setMessages([]llmwire.Message{
		usr("wait"),
		asst("", call("sleep-call-125", tool.IDSleep)),
		asst("", call("child-event", subagentEventTool)),
		{Role: llmwire.RoleTool, ToolCallID: "child-event", ToolName: subagentEventTool, Content: "done"},
	})

	r := stagedRunner(agent)
	done, err := r.handlePreviousResult(context.Background())
	require.NoError(t, err)
	assert.True(t, done)
	assert.True(t, r.result.Suspended)
	assert.Equal(t, int64(0), counter.runs.Load(), "the shadowed sleep must not execute again")
	assert.Equal(t, []PendingToolCall{{ID: "sleep-call-125", Name: tool.IDSleep}}, agent.PendingExternalCalls())
}

func TestResolvePendingCall_ExactAndIdempotent(t *testing.T) {
	agent := newTestAgent()
	agent.stagedCalls = map[string]string{"sleep-call-1": tool.IDSleep}
	agent.ms.setMessages([]llmwire.Message{
		asst("", call("sleep-call-1", tool.IDSleep)),
		asst("", call("newer-event", subagentEventTool)),
		{Role: llmwire.RoleTool, ToolCallID: "newer-event", ToolName: subagentEventTool, Content: "done"},
	})

	resolution, err := agent.ResolvePendingCall(
		context.Background(),
		PendingToolCall{ID: "sleep-call-1", Name: tool.IDSleep},
		"interrupted",
	)
	require.NoError(t, err)
	assert.Equal(t, CallResolutionInserted, resolution)

	resolution, err = agent.ResolvePendingCall(
		context.Background(),
		PendingToolCall{ID: "sleep-call-1", Name: tool.IDSleep},
		"duplicate delivery",
	)
	require.NoError(t, err)
	assert.Equal(t, CallResolutionAlreadyPresent, resolution)

	msgs := agent.ms.getMessages()
	var exactResults int
	for _, msg := range msgs {
		if msg.Role == llmwire.RoleTool && msg.ToolCallID == "sleep-call-1" {
			exactResults++
			assert.Equal(t, tool.IDSleep, msg.ToolName)
			assert.Equal(t, "interrupted", msg.Content)
		}
	}
	assert.Equal(t, 1, exactResults, "retry must not append a second result")
}

func TestSettleStoppedCalls_ClosesOnlyCurrentAndExternalCallsInTranscriptOrder(t *testing.T) {
	agent := newTestAgent()
	agent.stagedCalls = map[string]string{"external": tool.IDSleep}
	agent.ms.setMessages([]llmwire.Message{
		asst("", call("historical", "bash")),
		usr("new request supersedes the old bash"),
		asst("", call("external", tool.IDSleep)),
		asst("", call("ordinary-one", "bash"), call("ordinary-two", "read")),
		{Role: llmwire.RoleTool, ToolCallID: "ordinary-one", ToolName: "bash", Content: "already done"},
	})

	require.NoError(t, agent.SettleStoppedCalls(context.Background(), "Stopped by user."))
	require.NoError(t, agent.SettleStoppedCalls(context.Background(), "Stopped by user."))

	var settled []string
	for _, msg := range agent.ms.getMessages() {
		if msg.Role == llmwire.RoleTool && msg.Content == "Stopped by user." {
			settled = append(settled, msg.ToolCallID+":"+msg.ToolName)
		}
	}
	assert.Equal(t, []string{"external:sleep", "ordinary-two:read"}, settled)
}

func TestResolvePendingCall_RejectsDishonestIdentity(t *testing.T) {
	agent := newTestAgent()
	agent.stagedCalls = map[string]string{"sleep-call-1": tool.IDSleep}
	agent.ms.setMessages([]llmwire.Message{asst("", call("sleep-call-1", tool.IDSleep))})

	_, err := agent.ResolvePendingCall(
		context.Background(),
		PendingToolCall{ID: "sleep-call-1", Name: tool.IDTask},
		"wrong tool",
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tool name mismatch")

	_, err = agent.ResolvePendingCall(
		context.Background(),
		PendingToolCall{ID: "unknown", Name: tool.IDSleep},
		"wrong id",
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")

	agent.stagedCalls = map[string]string{"sleep-call-1": tool.IDTask}
	_, err = agent.ResolvePendingCall(
		context.Background(),
		PendingToolCall{ID: "sleep-call-1", Name: tool.IDSleep},
		"ledger disagrees",
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "producer name mismatch")
	assert.Equal(
		t,
		[]PendingToolCall{{ID: "sleep-call-1", Name: tool.IDTask}},
		agent.PendingExternalCalls(),
		"pending state exposes producer ownership so downstream routing also fails closed",
	)
}

func TestSyntheticInputsCannotJumpPendingExternalCall(t *testing.T) {
	agent := newTestAgent()
	agent.stagedCalls = map[string]string{"sleep-call-1": tool.IDSleep}
	agent.ms.setMessages([]llmwire.Message{asst("", call("sleep-call-1", tool.IDSleep))})

	_, err := agent.InjectToolNotificationOnce(context.Background(), "d1", subagentEventTool, "child done")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "still pending")

	_, err = agent.ResetContextAndInjectOnce(context.Background(), "d2", "fresh cron task")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "still pending")
	assert.Len(t, agent.ms.getMessages(), 1, "rejected inputs must not mutate the transcript")
}

// Once the verdict is injected the call is answered and the loop moves on.
func TestHandlePreviousResult_ResolvedStagedCallReleasesTheLoop(t *testing.T) {
	counter := &countingTool{id: tool.IDSetProvider}
	agent := newTestAgent(counter)
	agent.stagedCalls = map[string]string{"c1": tool.IDSetProvider}
	agent.ms.setMessages([]llmwire.Message{
		usr("add the provider"),
		asst("", call("c1", tool.IDSetProvider)),
		toolRes("c1"),
	})

	r := stagedRunner(agent)

	done, err := r.handlePreviousResult(context.Background())
	require.NoError(t, err)
	assert.False(t, done, "with the verdict in hand the loop calls the model again")
	assert.False(t, r.result.Suspended)
	assert.Equal(t, int64(0), counter.runs.Load())
}

// A call nobody staged has never run: re-executing it is the correct outcome, and
// is how a daemon that died before doing any work recovers.
func TestHandlePreviousResult_UnstagedCallStillExecutes(t *testing.T) {
	counter := &countingTool{id: tool.IDSetProvider}
	agent := newTestAgent(counter)
	agent.ms.setMessages([]llmwire.Message{usr("add it"), asst("", call("c1", tool.IDSetProvider))})

	r := stagedRunner(agent)

	done, err := r.handlePreviousResult(context.Background())
	require.NoError(t, err)
	assert.False(t, done)
	assert.Equal(t, int64(1), counter.runs.Load())
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
			staged: map[string]string{"c1": tool.IDSetManager},
			msgs:   []llmwire.Message{asst("", call("c1", tool.IDSetManager))},
			want:   true,
		},
		{
			name:   "staged and answered",
			staged: map[string]string{"c1": tool.IDSetManager},
			msgs:   []llmwire.Message{asst("", call("c1", tool.IDSetManager)), toolRes("c1")},
			want:   false,
		},
		{
			name:   "nothing staged",
			staged: nil,
			msgs:   []llmwire.Message{asst("", call("c1", tool.IDSetManager))},
			want:   false,
		},
		{
			name:   "staged in a superseded turn",
			staged: map[string]string{"c1": tool.IDSetManager},
			msgs: []llmwire.Message{
				asst("", call("c1", tool.IDSetManager)),
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
			agent.ms.setMessages(tt.msgs)

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
			msgs: []llmwire.Message{asst("", call("c1", tool.IDRemoveModel))},
			want: []string{"c1"},
		},
		{
			name: "an ordinary call is not",
			msgs: []llmwire.Message{asst("", call("c1", "read"))},
			want: nil,
		},
		{
			name: "protection survives a trailing user message",
			msgs: []llmwire.Message{asst("", call("c1", tool.IDSetProvider)), usr("hurry up")},
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
			agent.stagedCalls = tt.staged
			agent.ms.setMessages(tt.msgs)

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
		usr("add the provider"),
		asst("", call("c1", tool.IDSetProvider)),
		usr("hurry up"),
	}

	agent := newTestAgent()
	agent.ms.setMessages(msgs)

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
