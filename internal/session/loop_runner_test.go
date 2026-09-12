package session

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/transcript"
)

// loopScriptLLM drives runLoop with a scripted response sequence and records
// what the last call was handed. The last scripted response repeats.
type loopScriptLLM struct {
	responses []*llmwire.Response
	onCall    func(call int, msgs []llmwire.Message) (*llmwire.Response, error)
	err       error

	calls       int
	lastTools   []llmwire.ToolSchema
	lastOptions llmwire.ChatOptions
}

// loopNotifier captures everything the loop pushed to the human.
type loopNotifier struct {
	mu   sync.Mutex
	msgs []string
	err  error
}

// loopReloadStore serves the two store calls the loop itself can trip over:
// the per-iteration transcript reload and the compaction rewrite.
type loopReloadStore struct {
	mockSessionStore
	loadErr    error
	replaceErr error
}

type classificationBudgetGate struct {
	terminalBudgetGate
	types    []sessionstore.OutputType
	contents []string
	releases []bool
}

func (g *classificationBudgetGate) PersistResponse(
	_ context.Context,
	_ *transcript.Message,
	outputType sessionstore.OutputType,
	content string,
	releasesInput bool,
) (int64, bool, bool, error) {
	g.types = append(g.types, outputType)
	g.contents = append(g.contents, content)
	g.releases = append(g.releases, releasesInput)

	return int64(len(g.types)), false, outputType == sessionstore.OutputMessagePersistent, nil
}

func (g *classificationBudgetGate) PersistRejectedResponse(
	context.Context,
	sessionstore.RejectedResponse,
) (*sessionstore.RejectedResponseResult, error) {
	return &sessionstore.RejectedResponseResult{
		Outcome: sessionstore.RejectedResponseRecoveryQueued, RecoveryMessageID: 1,
	}, nil
}

type loopInputBoundary struct {
	agent    *svc
	input    *PendingInput
	onAccept func([]llmwire.Message)
}

func (b *loopInputBoundary) Peek(context.Context) (*PendingInput, error) {
	return b.input, nil
}

func (b *loopInputBoundary) Accept(
	ctx context.Context,
	_ PendingInput,
	prepared string,
	_ []PendingToolCall,
) (bool, bool, error) {
	if b.onAccept != nil {
		b.onAccept(b.agent.ms.getMessages())
	}
	if err := b.agent.ms.addUserMessage(ctx, prepared); err != nil {
		return false, false, err
	}
	b.input = nil

	return true, false, nil
}

func (b *loopInputBoundary) Reject(context.Context, PendingInput, string) error { return nil }
func (b *loopInputBoundary) Handle(context.Context, PendingInput, string) error {
	b.input = nil
	return nil
}

func (m *loopScriptLLM) Chat(
	_ context.Context,
	_ string,
	msgs []llmwire.Message,
	tools []llmwire.ToolSchema,
	opts ...llmwire.ChatOption,
) (*llmwire.Response, error) {
	m.calls++
	m.lastOptions = llmwire.ApplyChatOptions(opts)
	m.lastTools = tools

	if m.onCall != nil {
		response, err := m.onCall(m.calls, msgs)
		return normalizeScriptedResponse(response), err
	}

	if m.err != nil {
		return nil, m.err
	}

	return normalizeScriptedResponse(m.responses[min(m.calls-1, len(m.responses)-1)]), nil
}

func normalizeScriptedResponse(response *llmwire.Response) *llmwire.Response {
	if response == nil || response.FinishType != "" {
		return response
	}
	if len(response.ToolCalls) > 0 {
		response.FinishType = llmwire.FinishToolCalls
	} else {
		response.FinishType = llmwire.FinishStop
	}

	return response
}

func (m *loopScriptLLM) Model() string             { return testMockModel }
func (m *loopScriptLLM) APIKey() string            { return "" }
func (m *loopScriptLLM) Close() error              { return nil }
func (m *loopScriptLLM) Provider() string          { return testMockModel }
func (m *loopScriptLLM) ContextWindow() int        { return 0 }
func (m *loopScriptLLM) SetReasoningLevel(string)  {}
func (m *loopScriptLLM) GetReasoningLevel() string { return testReasoningLvl }
func (m *loopScriptLLM) SetSessionID(string)       {}

func (n *loopNotifier) fn(_ context.Context, msg string) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.msgs = append(n.msgs, msg)

	return n.err
}

func (n *loopNotifier) all() []string {
	n.mu.Lock()
	defer n.mu.Unlock()

	return append([]string(nil), n.msgs...)
}

func (n *loopNotifier) countWith(substr string) int {
	count := 0

	for _, m := range n.all() {
		if strings.Contains(m, substr) {
			count++
		}
	}

	return count
}

func (s *loopReloadStore) LoadActiveMessages(
	_ context.Context,
	_ int64,
) ([]*transcript.Message, error) {
	return nil, s.loadErr
}

func (s *loopReloadStore) ReplaceCompactedMessages(
	_ context.Context,
	_ int64,
	_ []int64,
	entries []sessionstore.CompactionEntry,
) ([]int64, error) {
	if s.replaceErr != nil {
		return nil, s.replaceErr
	}

	return make([]int64, len(entries)), nil
}

// summarizingLLM answers summarization calls (always a single message) with a
// brief that passes the quality gate, and every loop call with a final text.
func summarizingLLM() *loopScriptLLM {
	const brief = "## Goal\ng\n\n## Progress\np\n\n## Context for Continuation\nc"

	return &loopScriptLLM{onCall: func(_ int, msgs []llmwire.Message) (*llmwire.Response, error) {
		if len(msgs) == 1 {
			return textResponse(brief), nil
		}

		return textResponse("done"), nil
	}}
}

// iterationGuard bounds a run that is expected to stop by itself, so a broken
// iteration counter fails the test instead of hanging it.
func iterationGuard(limit int) iterationCallback {
	calls := 0

	return func(int, *llmwire.Response, []llmwire.ToolCall, bool) error {
		calls++
		if calls > limit {
			return fmt.Errorf("loop ran past %d iterations", limit)
		}

		return nil
	}
}

// loopRounds builds n complete rounds (assistant with one tool call plus its
// result), each result carrying bodySize bytes.
func loopRounds(n, bodySize int) []llmwire.Message {
	msgs := []llmwire.Message{{Role: llmwire.RoleUser, Content: "task"}}

	for i := range n {
		id := fmt.Sprintf("tc_%d", i)
		msgs = append(msgs,
			llmwire.Message{
				Role:      llmwire.RoleAssistant,
				ToolCalls: []llmwire.ToolCall{{ID: id, Name: "read", Arguments: []byte(`{}`)}},
			},
			llmwire.Message{
				Role:       llmwire.RoleTool,
				ToolCallID: id,
				ToolName:   "read",
				Content:    strings.Repeat("x", bodySize),
			},
		)
	}

	return msgs
}

func textResponse(text string) *llmwire.Response {
	return &llmwire.Response{Text: text, FinishType: llmwire.FinishStop}
}

func toolCallResponse(id, name string) *llmwire.Response {
	return &llmwire.Response{
		ToolCalls:  []llmwire.ToolCall{{ID: id, Name: name, Arguments: []byte(`{}`)}},
		FinishType: llmwire.FinishToolCalls,
	}
}

func TestRunLoopFinalTextResponseEndsRun(t *testing.T) {
	llmClient := &loopScriptLLM{responses: []*llmwire.Response{textResponse("all done")}}
	notifier := &loopNotifier{}

	agent := newTestAgent()
	agent.llmClient = llmClient

	result, err := runLoop(t.Context(), agent, loopOptions{Notify: notifier.fn}, iterationGuard(5))

	require.NoError(t, err)
	assert.Equal(t, "all done", result.FinalResponse)
	assert.Equal(t, 1, result.Iterations)
	assert.Equal(t, []string{"all done"}, notifier.all())
}

func TestRunLoopDoesNotRepublishPreviousFinalBeforeAcceptingNextInput(t *testing.T) {
	agent := newTestAgent()
	agent.ms.setMessages([]llmwire.Message{
		usr("old question"),
		asst("old answer"),
	})
	agent.boundary = &loopInputBoundary{
		agent: agent,
		input: &PendingInput{ID: 1, Content: "new question", ReceivedAt: time.Now()},
	}

	llmClient := &loopScriptLLM{onCall: func(_ int, msgs []llmwire.Message) (*llmwire.Response, error) {
		require.Equal(t, llmwire.RoleUser, msgs[len(msgs)-1].Role)
		require.Contains(t, msgs[len(msgs)-1].Content, "new question")
		return textResponse("new answer"), nil
	}}
	agent.llmClient = llmClient
	notifier := &loopNotifier{}

	result, err := runLoop(t.Context(), agent, loopOptions{Notify: notifier.fn}, iterationGuard(5))

	require.NoError(t, err)
	assert.Equal(t, "new answer", result.FinalResponse)
	assert.Equal(t, 1, llmClient.calls)
	assert.Zero(t, notifier.countWith("old answer"))
	assert.Equal(t, 1, notifier.countWith("new answer"))
}

func TestRunLoopExecutesPreviousToolsBeforeAcceptingNextInput(t *testing.T) {
	agent := newTestAgent(&stubTool{id: "read", result: "tool result"})
	agent.ms.setMessages([]llmwire.Message{
		usr("old task"),
		asst("", call("read-1", "read")),
	})

	toolSettled := false
	agent.boundary = &loopInputBoundary{
		agent: agent,
		input: &PendingInput{ID: 1, Content: "new detail", ReceivedAt: time.Now()},
		onAccept: func(msgs []llmwire.Message) {
			toolSettled = slices.ContainsFunc(msgs, func(msg llmwire.Message) bool {
				return msg.Role == llmwire.RoleTool && msg.ToolCallID == "read-1"
			})
		},
	}
	agent.llmClient = &loopScriptLLM{responses: []*llmwire.Response{textResponse("done")}}

	_, err := runLoop(t.Context(), agent, loopOptions{}, iterationGuard(5))

	require.NoError(t, err)
	assert.True(t, toolSettled, "the older tool result must exist before the queued user input is promoted")
}

func TestRunLoopHandlesStatusAtBoundaryWithoutCallingModel(t *testing.T) {
	agent := newTestAgent()
	agent.boundary = &loopInputBoundary{
		agent: agent,
		input: &PendingInput{ID: 1, Content: "/status", ReceivedAt: time.Now()},
	}
	llmClient := &loopScriptLLM{responses: []*llmwire.Response{textResponse("must not run")}}
	agent.llmClient = llmClient
	agent.activeBackgroundSnapshot = "\n\n# Active background work\nprocess bgp_1"
	notifier := &loopNotifier{}

	_, err := runLoop(t.Context(), agent, loopOptions{Notify: notifier.fn}, iterationGuard(5))

	require.NoError(t, err)
	assert.Zero(t, llmClient.calls)
	assert.Empty(t, agent.ms.getMessages())
	assert.Equal(t, 1, notifier.countWith("Session Status"))
}

func TestCallLLMAppendsActiveBackgroundSnapshotOncePerActivation(t *testing.T) {
	const snapshot = "\n\n# Active background work\nprocess bgp_1"

	seen := make([][]llmwire.Message, 0, 2)
	agent := newTestAgent()
	agent.activeBackgroundSnapshot = snapshot
	agent.llmClient = &loopScriptLLM{onCall: func(_ int, msgs []llmwire.Message) (*llmwire.Response, error) {
		seen = append(seen, append([]llmwire.Message(nil), msgs...))
		return textResponse("done"), nil
	}}
	runner := &loopRunner{agent: agent, result: &loopResult{}, log: zap.NewNop()}

	require.NoError(t, runner.callLLM(t.Context()))
	require.NoError(t, runner.callLLM(t.Context()))

	require.Len(t, seen, 2)
	for _, messages := range seen {
		count := 0
		for _, msg := range messages {
			if msg.Role == llmwire.RoleUser && msg.Content == snapshot {
				count++
			}
		}
		assert.Equal(t, 1, count)
	}
	count := 0
	for _, msg := range agent.ms.getMessages() {
		if msg.Role == llmwire.RoleUser && msg.Content == snapshot {
			count++
		}
	}
	assert.Equal(t, 1, count)
}

func TestCallLLMAppendsActiveBackgroundSnapshotAgainOnLaterActivation(t *testing.T) {
	agent := newTestAgent()
	agent.activeBackgroundSnapshot = "\n\n# Active background work\nprocess bgp_1"
	agent.llmClient = &loopScriptLLM{responses: []*llmwire.Response{textResponse("done")}}

	for range 2 {
		runner := &loopRunner{agent: agent, result: &loopResult{}, log: zap.NewNop()}
		require.NoError(t, runner.callLLM(t.Context()))
	}

	count := 0
	for _, msg := range agent.ms.getMessages() {
		if msg.Role == llmwire.RoleUser && msg.Content == agent.activeBackgroundSnapshot {
			count++
		}
	}
	assert.Equal(t, 2, count)
}

func TestCallLLMFailsBeforeProviderWhenBackgroundSnapshotPersistenceFails(t *testing.T) {
	store := &mockSessionStore{insertErr: errors.New("disk full")}
	agent := newTestAgent()
	agent.ms = newMessageStore(store, 1, nil)
	agent.activeBackgroundSnapshot = "\n\n# Active background work\nprocess bgp_1"
	client := &loopScriptLLM{responses: []*llmwire.Response{textResponse("must not run")}}
	agent.llmClient = client
	runner := &loopRunner{agent: agent, result: &loopResult{}, log: zap.NewNop()}

	err := runner.callLLM(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "record active background snapshot")
	assert.Zero(t, client.calls)
	assert.Empty(t, agent.ms.getMessages())
	assert.False(t, runner.backgroundInserted)
}

func TestAssistantOutput_DirectReplyPrecedesReplaceableProgress(t *testing.T) {
	tests := []struct {
		name         string
		response     *llmwire.Response
		enabled      bool
		replyToInput bool
		directReply  bool
		wantType     sessionstore.OutputType
		wantOutput   string
	}{
		{name: "output disabled", response: textResponse("done")},
		{name: "empty response", response: &llmwire.Response{}, enabled: true, replyToInput: true},
		{
			name: "internal progress narration", response: &llmwire.Response{
				Text: "checking", ToolCalls: []llmwire.ToolCall{call("internal", "read")},
			}, enabled: true,
		},
		{
			name: "direct reply with tool", response: &llmwire.Response{
				Text: "stopping the mutation run", ToolCalls: []llmwire.ToolCall{call("reply", "bash")},
			}, enabled: true, replyToInput: true, directReply: true,
			wantType: sessionstore.OutputMessagePersistent, wantOutput: "stopping the mutation run",
		},
		{
			name: "later manager progress narration", response: &llmwire.Response{
				Text: "still working", ToolCalls: []llmwire.ToolCall{call("later", "read")},
			}, enabled: true, replyToInput: true,
		},
		{
			name: "asynchronous terminal progress", response: textResponse("done"), enabled: true,
			wantType: sessionstore.OutputMessageReplaceable, wantOutput: "done",
		},
		{
			name: "manager-owned terminal answer", response: textResponse("done"), enabled: true,
			replyToInput: true,
			wantType:     sessionstore.OutputMessagePersistent, wantOutput: "done",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			outputType, output := assistantOutput(tc.response, tc.enabled, tc.replyToInput, tc.directReply)
			assert.Equal(t, tc.wantType, outputType)
			assert.Equal(t, tc.wantOutput, output)
		})
	}
}

func TestRecordIterationBudgetedConsumesDirectReplyButKeepsTerminalObligation(t *testing.T) {
	agent := newTestAgent()
	agent.outputEnabled = true
	gate := &classificationBudgetGate{}
	agent.budgetGate = gate
	runner := &loopRunner{
		agent: agent, result: &loopResult{}, log: zap.NewNop(),
		replyToInput: true, directReplyEligible: true,
	}

	responses := []*llmwire.Response{
		{Text: "first", ToolCalls: []llmwire.ToolCall{call("one", "read")}, FinishType: llmwire.FinishToolCalls},
		{Text: "second", ToolCalls: []llmwire.ToolCall{call("two", "read")}, FinishType: llmwire.FinishToolCalls},
		textResponse("done"),
	}
	for _, response := range responses {
		runner.lastResp = response
		require.NoError(t, runner.recordIteration(t.Context()))
	}

	assert.Equal(t, []sessionstore.OutputType{sessionstore.OutputMessagePersistent, "", ""}, gate.types)
	assert.Equal(t, []string{"first", "", ""}, gate.contents)
	assert.Equal(t, []bool{false, false, true}, gate.releases)
	assert.False(t, runner.replyToInput)
	assert.False(t, runner.directReplyEligible)
}

func TestRecordIterationRejectedResponseConsumesDirectReplyEligibility(t *testing.T) {
	agent := newTestAgent()
	agent.budgetGate = &classificationBudgetGate{}
	runner := &loopRunner{
		agent: agent, result: &loopResult{}, log: zap.NewNop(),
		replyToInput: true, directReplyEligible: true,
		lastResp: &llmwire.Response{Text: "truncated", FinishType: llmwire.FinishLength},
	}

	require.NoError(t, runner.recordIteration(t.Context()))
	assert.True(t, runner.replyToInput)
	assert.False(t, runner.directReplyEligible)

	runner.lastResp = &llmwire.Response{
		Text: "recovering", ToolCalls: []llmwire.ToolCall{call("recover", "read")},
		FinishType: llmwire.FinishToolCalls,
	}
	require.NoError(t, runner.recordIteration(t.Context()))
	assert.True(t, runner.replyToInput)
	assert.False(t, runner.publishedReply)
}

// TestRunLoopStopsExactlyAtHardCeiling pins the only loop termination cap: the
// internal 1000-iteration defect breaker, with no configurable lower path.
func TestRunLoopStopsExactlyAtHardCeiling(t *testing.T) {
	llmClient := &loopScriptLLM{onCall: func(call int, _ []llmwire.Message) (*llmwire.Response, error) {
		return toolCallResponse(fmt.Sprintf("tc_%d", call), "read"), nil
	}}

	agent := newTestAgent(&stubTool{id: "read", result: "content"})
	agent.llmClient = llmClient

	result, err := runLoop(t.Context(), agent, loopOptions{}, iterationGuard(hardIterationCeiling))

	require.Error(t, err)
	require.EqualError(t, err, fmt.Sprintf("maximum iterations (%d) reached", hardIterationCeiling))
	assert.Equal(t, hardIterationCeiling, result.Iterations)
}

func TestRunLoopCancelledContextStopsBeforeFirstCall(t *testing.T) {
	llmClient := &loopScriptLLM{responses: []*llmwire.Response{textResponse("never")}}

	agent := newTestAgent()
	agent.llmClient = llmClient

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	result, err := runLoop(ctx, agent, loopOptions{}, nil)

	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, result.Error, context.Canceled)
	assert.Equal(t, 0, llmClient.calls)
	assert.Equal(t, 0, result.Iterations)
}

func TestRunLoopAnnouncesAssistantTextBeforeExecutingItsTools(t *testing.T) {
	llmClient := &loopScriptLLM{responses: []*llmwire.Response{
		{Text: "working on it", ToolCalls: []llmwire.ToolCall{{ID: "tc_1", Name: "read", Arguments: []byte(`{}`)}}},
		textResponse("done"),
	}}
	notifier := &loopNotifier{}

	agent := newTestAgent(&stubTool{id: "read", result: "content"})
	agent.llmClient = llmClient

	result, err := runLoop(t.Context(), agent, loopOptions{Notify: notifier.fn}, iterationGuard(5))

	require.NoError(t, err)
	assert.Equal(t, "done", result.FinalResponse)
	assert.Equal(t, []string{"🔄 working on it", "done"}, notifier.all())
}

func TestRunLoopSilentToolTurnNotifiesNothing(t *testing.T) {
	llmClient := &loopScriptLLM{responses: []*llmwire.Response{
		toolCallResponse("tc_1", "read"),
		textResponse("done"),
	}}
	notifier := &loopNotifier{}

	agent := newTestAgent(&stubTool{id: "read", result: "content"})
	agent.llmClient = llmClient

	_, err := runLoop(t.Context(), agent, loopOptions{Notify: notifier.fn}, iterationGuard(5))

	require.NoError(t, err)
	assert.Equal(t, []string{"done"}, notifier.all())
}

func TestRunLoopEmptyResponseLadder(t *testing.T) {
	llmClient := &loopScriptLLM{responses: []*llmwire.Response{{}}}
	notifier := &loopNotifier{}

	agent := newTestAgent()
	agent.llmClient = llmClient

	result, err := runLoop(t.Context(), agent, loopOptions{Notify: notifier.fn}, iterationGuard(20))

	require.NoError(t, err)

	// The sixth empty response is observed on the seventh pass, which returns
	// before another LLM call — hence six recorded iterations.
	assert.Equal(t, 6, result.Iterations)
	assert.Equal(t, []string{
		"⚠️ Model returned 6 consecutive empty responses. Session paused — waiting for input.",
	}, notifier.all())

	var nudges []string

	for _, msg := range agent.ms.getMessages() {
		if msg.Role == llmwire.RoleUser {
			nudges = append(nudges, msg.Content)
		}
	}

	plain := "You returned an empty response with no tool calls. " +
		"Please continue working on the task, or explain what you need."
	warn := "[AUTOMATED WARNING: You have returned 3 consecutive empty responses (no text, no tool calls). " +
		"You MUST either use a tool or respond with text. If you cannot proceed, explain why.]"

	assert.Equal(t, []string{plain, plain, warn, plain, plain}, nudges)
}

func TestRunLoopEmptyResponseStreakResetByProductiveTurn(t *testing.T) {
	llmClient := &loopScriptLLM{onCall: func(call int, _ []llmwire.Message) (*llmwire.Response, error) {
		switch call {
		case 3:
			return toolCallResponse("tc_break", "read"), nil
		case 4:
			return textResponse("done"), nil
		default:
			return &llmwire.Response{}, nil
		}
	}}

	agent := newTestAgent(&stubTool{id: "read", result: "content"})
	agent.llmClient = llmClient

	result, err := runLoop(t.Context(), agent, loopOptions{}, iterationGuard(10))

	require.NoError(t, err)
	assert.Equal(t, "done", result.FinalResponse)

	for _, msg := range agent.ms.getMessages() {
		assert.NotContains(t, msg.Content, "AUTOMATED WARNING")
	}
}

func TestRunLoopEmptyResponseNudgeRecordFailureAborts(t *testing.T) {
	store := &mockSessionStore{insertErr: errors.New("disk full"), insertFailAt: 2}
	llmClient := &loopScriptLLM{responses: []*llmwire.Response{{}}}

	agent := newTestAgent()
	agent.llmClient = llmClient
	agent.ms = newMessageStore(store, 1, nil)

	result, err := runLoop(t.Context(), agent, loopOptions{}, iterationGuard(20))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "record empty-response nudge")
	require.ErrorIs(t, result.Error, store.insertErr)
}

func TestRunLoopSuspendedToolEndsRunWithoutError(t *testing.T) {
	llmClient := &loopScriptLLM{responses: []*llmwire.Response{toolCallResponse("tc_1", "sleep")}}

	agent := newTestAgent(&stubTool{id: "sleep", err: tool.ErrSuspend})
	agent.llmClient = llmClient

	result, err := runLoop(t.Context(), agent, loopOptions{}, iterationGuard(5))

	require.NoError(t, err)
	assert.True(t, result.Suspended)
	assert.Equal(t, 1, result.Iterations)
	assert.Equal(t, 1, llmClient.calls)
}

func TestRunLoopPendingToolRecordFailureAborts(t *testing.T) {
	store := &mockSessionStore{insertErr: errors.New("disk full"), insertFailAt: 2}
	llmClient := &loopScriptLLM{responses: []*llmwire.Response{toolCallResponse("tc_1", "read")}}

	agent := newTestAgent(&stubTool{id: "read", result: "content"})
	agent.llmClient = llmClient
	agent.ms = newMessageStore(store, 1, nil)

	result, err := runLoop(t.Context(), agent, loopOptions{}, iterationGuard(5))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "execute pending tools")
	require.ErrorIs(t, result.Error, store.insertErr)
	assert.False(t, result.Suspended)
}

// The threshold has exactly one answer: compaction. Clearing happens inside it,
// so it never surfaces as an event of its own.
func TestRunLoopThresholdCompactsWithoutAClearEvent(t *testing.T) {
	notifier := &loopNotifier{}

	agent := newTestAgent()
	agent.llmClient = summarizingLLM()
	agent.ms.setMessages(loopRounds(70, 4000))

	require.True(t, agent.shouldCompact(compactionThreshold))

	_, err := runLoop(t.Context(), agent, loopOptions{Notify: notifier.fn}, iterationGuard(5))
	require.NoError(t, err)

	assert.Equal(t, 0, notifier.countWith("🧹"), "clearing is a phase of compaction, not a user-visible event")
	assert.Equal(t, 1, notifier.countWith("🔄 Compacting context..."))
	assert.Equal(t, 1, notifier.countWith("✅ Context compacted"))
}

func TestRunLoopExplicitCompactionForcesSummarization(t *testing.T) {
	notifier := &loopNotifier{}

	agent := newTestAgent()
	agent.llmClient = summarizingLLM()
	agent.ms.setMessages(loopRounds(70, 4000))
	agent.RequestCompaction()

	_, err := runLoop(t.Context(), agent, loopOptions{Notify: notifier.fn}, iterationGuard(5))
	require.NoError(t, err)

	assert.Equal(t, 0, notifier.countWith("🧹 Cleared"), "clearing never surfaces on its own")
	assert.Equal(t, 1, notifier.countWith("🔄 Compacting context..."))
	assert.Equal(t, 1, notifier.countWith("✅ Context compacted"))
	assert.Equal(t, 0, notifier.countWith("❌ Compaction failed"))
}

func TestRunLoopCompactionFailureIsReportedAndSurvived(t *testing.T) {
	notifier := &loopNotifier{}

	agent := newTestAgent()
	agent.llmClient = summarizingLLM()
	agent.ms = newMessageStore(&loopReloadStore{replaceErr: errors.New("write conflict")}, 1, nil)
	agent.ms.setMessages(loopRounds(70, 4000))
	agent.RequestCompaction()

	result, err := runLoop(t.Context(), agent, loopOptions{Notify: notifier.fn}, iterationGuard(5))

	require.NoError(t, err, "a rejected rewrite must not take the run down with it")
	assert.Equal(t, "done", result.FinalResponse)
	assert.Equal(t, 1, notifier.countWith("❌ Compaction failed"))
	assert.Equal(t, 0, notifier.countWith("✅ Context compacted"))
}

func TestRunLoopCompactionWithNothingToSummarizeStaysSilent(t *testing.T) {
	llmClient := &loopScriptLLM{responses: []*llmwire.Response{textResponse("done")}}
	notifier := &loopNotifier{}

	agent := newTestAgent()
	agent.llmClient = llmClient
	agent.ms.setMessages([]llmwire.Message{{Role: llmwire.RoleUser, Content: strings.Repeat("x", 400000)}})

	_, err := runLoop(t.Context(), agent, loopOptions{Notify: notifier.fn}, iterationGuard(5))
	require.NoError(t, err)

	// The verbatim tail is never empty (D3), so this transcript can never yield
	// a split: the auto path pre-checks that and stays fully silent instead of
	// announcing an attempt it can never make.
	assert.Equal(t, 0, notifier.countWith("🔄 Compacting context..."))
	assert.Equal(t, 0, notifier.countWith("✅ Context compacted"))
	assert.Equal(t, 0, notifier.countWith("❌ Compaction failed"))
	assert.Equal(t, 0, notifier.countWith("Nothing to compact"), "the auto path stays quiet")
	assert.Equal(t, 1, llmClient.calls, "no summarization call was made")
}

func TestRunLoopForcedTextOnlyWithholdsToolsFromModel(t *testing.T) {
	tests := []struct {
		name          string
		forceTextOnly bool
		wantTools     int
	}{
		{name: "normal mode advertises tools", forceTextOnly: false, wantTools: 2},
		{name: "forced text-only advertises none", forceTextOnly: true, wantTools: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			llmClient := &loopScriptLLM{responses: []*llmwire.Response{textResponse("done")}}

			agent := newTestAgent(
				&stubTool{id: "read", result: "a"},
				&stubTool{id: "grep", result: "b"},
			)
			agent.llmClient = llmClient
			agent.loopDetector.forceTextOnly = tt.forceTextOnly

			_, err := runLoop(t.Context(), agent, loopOptions{}, iterationGuard(5))
			require.NoError(t, err)

			assert.Len(t, llmClient.lastTools, tt.wantTools)
		})
	}
}

func TestRunLoopForcedTextOnlyClearedOnlyByTextResponse(t *testing.T) {
	tests := []struct {
		name     string
		response *llmwire.Response
		want     bool
		wantErr  bool
	}{
		{name: "text response releases the model", response: textResponse("explaining myself"), want: false},
		{name: "tool call keeps the clamp on", response: toolCallResponse("tc_1", "read"), want: true, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			llmClient := &loopScriptLLM{responses: []*llmwire.Response{tt.response}}

			agent := newTestAgent(&stubTool{id: "read", result: "a"})
			agent.llmClient = llmClient
			agent.loopDetector.forceTextOnly = true

			// The guard bounds the tool-call case; the text case ends by itself
			// before the guard trips.
			_, err := runLoop(t.Context(), agent, loopOptions{}, iterationGuard(1))
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			assert.Equal(t, tt.want, agent.loopDetector.forceTextOnly)
		})
	}
}

func TestRunLoopLLMErrorSurfacesToCallerAndUser(t *testing.T) {
	boom := errors.New("boom")
	llmClient := &loopScriptLLM{err: boom}
	notifier := &loopNotifier{}

	agent := newTestAgent()
	agent.llmClient = llmClient

	result, err := runLoop(t.Context(), agent, loopOptions{Notify: notifier.fn}, iterationGuard(5))

	require.ErrorIs(t, err, boom)
	assert.Contains(t, err.Error(), "LLM call failed")
	require.ErrorIs(t, result.Error, boom)
	assert.Equal(t, "❌ LLM error: boom", result.ErrorNotice)
	assert.Empty(t, notifier.all(), "the daemon publishes only after the error state and outbox commit")
	assert.Empty(t, agent.ms.getMessages(), "a failed call records nothing")
}

func TestRunLoopCallbackSeesNumberedIterationAndToolCalls(t *testing.T) {
	response := toolCallResponse("tc_1", "read")
	llmClient := &loopScriptLLM{onCall: func(call int, _ []llmwire.Message) (*llmwire.Response, error) {
		if call == 1 {
			return response, nil
		}

		return textResponse("done"), nil
	}}

	agent := newTestAgent(&stubTool{id: "read", result: "content"})
	agent.llmClient = llmClient

	var (
		iterations []int
		seenCalls  [][]llmwire.ToolCall
	)

	cb := func(iteration int, resp *llmwire.Response, toolCalls []llmwire.ToolCall, _ bool) error {
		iterations = append(iterations, iteration)
		seenCalls = append(seenCalls, toolCalls)
		assert.NotNil(t, resp)

		return nil
	}

	_, err := runLoop(t.Context(), agent, loopOptions{}, cb)
	require.NoError(t, err)

	assert.Equal(t, []int{1, 2}, iterations)
	require.Len(t, seenCalls, 2)
	assert.Equal(t, response.ToolCalls, seenCalls[0])
	assert.Empty(t, seenCalls[1])
}

func TestRunLoopCallbackFailureAbortsBeforeRecordingTurn(t *testing.T) {
	llmClient := &loopScriptLLM{responses: []*llmwire.Response{textResponse("done")}}

	agent := newTestAgent()
	agent.llmClient = llmClient

	cbErr := errors.New("checkpoint failed")
	cb := func(int, *llmwire.Response, []llmwire.ToolCall, bool) error { return cbErr }

	result, err := runLoop(t.Context(), agent, loopOptions{}, cb)

	require.ErrorIs(t, err, cbErr)
	assert.Contains(t, err.Error(), "iteration callback failed")
	require.ErrorIs(t, result.Error, cbErr)
	assert.Equal(t, 1, result.Iterations)
	assert.Empty(t, agent.ms.getMessages(), "the turn is not recorded when the checkpoint fails")
}

func TestRunLoopAssistantRecordFailureAborts(t *testing.T) {
	store := &mockSessionStore{insertErr: errors.New("disk full"), insertFailAt: 1}
	llmClient := &loopScriptLLM{responses: []*llmwire.Response{textResponse("done")}}

	agent := newTestAgent()
	agent.llmClient = llmClient
	agent.ms = newMessageStore(store, 1, nil)

	result, err := runLoop(t.Context(), agent, loopOptions{}, iterationGuard(5))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "record assistant message")
	require.ErrorIs(t, result.Error, store.insertErr)
}

// finalizingRunner builds a loopRunner for the hard-breaker terminal routine:
// last-text promotion, final-output enqueue, notification, and the ceiling error.
func finalizingRunner(ctx context.Context, agent *svc, opts loopOptions) *loopRunner {
	return &loopRunner{
		agent:  agent,
		opts:   opts,
		result: &loopResult{},
		log:    logger.Ctx(ctx).Named("session.loop"),
	}
}

func TestRunLoopFinalizeRecoversLastAssistantText(t *testing.T) {
	notifier := &loopNotifier{}

	agent := newTestAgent()
	agent.ms.setMessages([]llmwire.Message{
		{Role: llmwire.RoleUser, Content: "task"},
		{Role: llmwire.RoleAssistant, Content: "partial answer"},
	})

	r := finalizingRunner(t.Context(), agent, loopOptions{Notify: notifier.fn})
	result, err := r.finalize(t.Context())

	require.Error(t, err)
	require.EqualError(t, err, fmt.Sprintf("maximum iterations (%d) reached", hardIterationCeiling))
	assert.Equal(t, "partial answer", result.FinalResponse)
	assert.Equal(t, []string{"partial answer"}, notifier.all())
}

func TestRunLoopFinalizeNotifyFailureIsLogged(t *testing.T) {
	tests := []struct {
		name      string
		notifyErr error
		wantLog   int
	}{
		{name: "delivered final answer is silent", notifyErr: nil, wantLog: 0},
		{name: "undelivered final answer warns", notifyErr: errors.New("telegram down"), wantLog: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			notifier := &loopNotifier{err: tt.notifyErr}

			agent := newTestAgent()
			agent.ms.setMessages([]llmwire.Message{
				{Role: llmwire.RoleUser, Content: "task"},
				{Role: llmwire.RoleAssistant, Content: "partial answer"},
			})

			core, logs := observer.New(zapcore.WarnLevel)
			ctx := logger.ToContext(t.Context(), zap.New(core))

			r := finalizingRunner(ctx, agent, loopOptions{Notify: notifier.fn})
			result, err := r.finalize(ctx)

			require.Error(t, err) // the hard-breaker terminal path always fails the run
			assert.Equal(t, "partial answer", result.FinalResponse)
			assert.Equal(t, []string{"partial answer"}, notifier.all())
			assert.Len(t, logs.FilterMessage("notify_failed").All(), tt.wantLog)
		})
	}
}

func TestRunLoopFinalizeIgnoresBlankAssistantText(t *testing.T) {
	notifier := &loopNotifier{}

	agent := newTestAgent()
	agent.ms.setMessages([]llmwire.Message{
		{Role: llmwire.RoleUser, Content: "task"},
		{Role: llmwire.RoleAssistant, Content: "   "},
	})

	r := finalizingRunner(t.Context(), agent, loopOptions{Notify: notifier.fn})
	result, err := r.finalize(t.Context())

	require.Error(t, err)
	assert.Empty(t, result.FinalResponse, "whitespace is not an answer")
	assert.Empty(t, notifier.all())
}

func TestRunLoopIterationStartIsOneBased(t *testing.T) {
	llmClient := &loopScriptLLM{onCall: func(call int, _ []llmwire.Message) (*llmwire.Response, error) {
		if call == 1 {
			return toolCallResponse("tc_1", "read"), nil
		}

		return textResponse("done"), nil
	}}

	agent := newTestAgent(&stubTool{id: "read", result: "content"})
	agent.llmClient = llmClient

	core, logs := observer.New(zapcore.InfoLevel)
	ctx := logger.ToContext(t.Context(), zap.New(core))

	_, err := runLoop(ctx, agent, loopOptions{}, iterationGuard(5))
	require.NoError(t, err)

	entries := logs.FilterMessage("iteration_start").All()
	require.Len(t, entries, 3)

	var seen []any
	for _, e := range entries {
		seen = append(seen, e.ContextMap()["iter"])
	}

	assert.Equal(t, []any{int64(1), int64(2), int64(3)}, seen)
}

func TestRunLoop_AsyncInputArrivingDuringModelCallWaitsForNextSafeBoundary(t *testing.T) {
	read := &countingTool{id: "read"}
	agent := newTestAgent(read)
	boundary := &loopInputBoundary{agent: agent}
	agent.boundary = boundary
	agent.llmClient = &loopScriptLLM{onCall: func(call int, messages []llmwire.Message) (*llmwire.Response, error) {
		switch call {
		case 1:
			assert.False(t, hasMessageContent(messages, "<process_completion>"))
			boundary.input = &PendingInput{
				ID: 1, Content: "<process_completion>\nprocess_id: bgp_1\n</process_completion>",
				Source: sessionstore.InputSourceProcess, ReceivedAt: time.Now(),
			}

			return toolCallResponse("read-before-completion", "read"), nil
		case 2:
			require.Len(t, messages, 3)
			assert.Equal(t, llmwire.RoleAssistant, messages[0].Role)
			assert.Equal(t, llmwire.RoleTool, messages[1].Role)
			assert.Equal(t, llmwire.RoleUser, messages[2].Role)
			assert.Contains(t, messages[2].Content, "<process_completion>")

			return textResponse("done"), nil
		default:
			return nil, fmt.Errorf("unexpected model call %d", call)
		}
	}}

	result, err := runLoop(t.Context(), agent, loopOptions{}, iterationGuard(5))
	require.NoError(t, err)
	assert.Equal(t, "done", result.FinalResponse)
	assert.Equal(t, int64(1), read.runs.Load())
}

func hasMessageContent(messages []llmwire.Message, fragment string) bool {
	return slices.ContainsFunc(messages, func(message llmwire.Message) bool {
		return strings.Contains(message.Content, fragment)
	})
}

func TestRunLoopReloadFailureIsLoggedAndSurvived(t *testing.T) {
	tests := []struct {
		name    string
		loadErr error
		wantLog bool
	}{
		{name: "successful reload is silent", loadErr: nil, wantLog: false},
		{name: "failed reload warns and continues", loadErr: errors.New("db gone"), wantLog: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			llmClient := &loopScriptLLM{responses: []*llmwire.Response{textResponse("done")}}

			agent := newTestAgent()
			agent.llmClient = llmClient
			agent.ms = newMessageStore(&loopReloadStore{loadErr: tt.loadErr}, 1, nil)

			core, logs := observer.New(zapcore.WarnLevel)
			ctx := logger.ToContext(t.Context(), zap.New(core))

			result, err := runLoop(ctx, agent, loopOptions{}, iterationGuard(5))

			require.NoError(t, err)
			assert.Equal(t, "done", result.FinalResponse)
			assert.Len(
				t,
				logs.FilterMessage("reload_messages_failed").All(),
				map[bool]int{true: 1, false: 0}[tt.wantLog],
			)
		})
	}
}

func TestRunLoopNotifyFailureIsLoggedNotFatal(t *testing.T) {
	tests := []struct {
		name      string
		notifyErr error
		wantLog   bool
	}{
		{name: "delivered notification is silent", notifyErr: nil, wantLog: false},
		{name: "undelivered notification warns", notifyErr: errors.New("telegram down"), wantLog: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			llmClient := &loopScriptLLM{responses: []*llmwire.Response{textResponse("done")}}
			notifier := &loopNotifier{err: tt.notifyErr}

			agent := newTestAgent()
			agent.llmClient = llmClient

			core, logs := observer.New(zapcore.WarnLevel)
			ctx := logger.ToContext(t.Context(), zap.New(core))

			result, err := runLoop(ctx, agent, loopOptions{Notify: notifier.fn}, iterationGuard(5))

			require.NoError(t, err)
			assert.Equal(t, "done", result.FinalResponse, "a lost notification does not lose the answer")
			assert.Len(t, logs.FilterMessage("notify_failed").All(), map[bool]int{true: 1, false: 0}[tt.wantLog])
		})
	}
}

func TestRunLoopLogsIterationCostOnlyWhenCharged(t *testing.T) {
	tests := []struct {
		name    string
		cost    float64
		wantLog bool
		wantVal string
	}{
		{name: "free turn is not logged", cost: 0, wantLog: false},
		{name: "charged turn is logged", cost: 0.25, wantLog: true, wantVal: "$0.2500"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			llmClient := &loopScriptLLM{responses: []*llmwire.Response{
				{Text: "done", CostUSD: tt.cost},
			}}

			agent := newTestAgent()
			agent.llmClient = llmClient

			core, logs := observer.New(zapcore.InfoLevel)
			ctx := logger.ToContext(t.Context(), zap.New(core))

			_, err := runLoop(ctx, agent, loopOptions{}, iterationGuard(5))
			require.NoError(t, err)

			entries := logs.FilterMessage("iteration_cost").All()

			if !tt.wantLog {
				assert.Empty(t, entries)

				return
			}

			require.Len(t, entries, 1)
			assert.Equal(t, tt.wantVal, entries[0].ContextMap()["cost_usd"])
		})
	}
}

func TestLastAssistantStateEdges(t *testing.T) {
	tests := []struct {
		name     string
		messages []llmwire.Message
		want     *assistantState
	}{
		{
			// No assistant anywhere: the scan must fall through, not pick a row by index.
			name: "tool rows only",
			messages: []llmwire.Message{
				{Role: llmwire.RoleTool, ToolCallID: "tc_1", ToolName: "read", Content: "a"},
				{Role: llmwire.RoleTool, ToolCallID: "tc_2", ToolName: "read", Content: "b"},
			},
			want: nil,
		},
		{
			name: "assistant is the very first message",
			messages: []llmwire.Message{
				{Role: llmwire.RoleAssistant, Content: "opening line"},
			},
			want: &assistantState{HasText: true, Text: "opening line"},
		},
		{
			// A result recorded before the current turn must not resolve a call the
			// last assistant re-issued under the same id — that would silently drop it.
			name: "recycled tool_call id from an earlier round",
			messages: []llmwire.Message{
				{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "tc_1", Name: "read"}}},
				{Role: llmwire.RoleTool, ToolCallID: "tc_1", ToolName: "read", Content: "stale"},
				{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "tc_1", Name: "read"}}},
			},
			want: &assistantState{
				HasPendingTools: true,
				PendingTools:    []llmwire.ToolCall{{ID: "tc_1", Name: "read"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, lastAssistantState(tt.messages))
		})
	}
}
