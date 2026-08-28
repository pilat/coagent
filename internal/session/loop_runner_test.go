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
		return m.onCall(m.calls, msgs)
	}

	if m.err != nil {
		return nil, m.err
	}

	return m.responses[min(m.calls-1, len(m.responses)-1)], nil
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
) ([]*sessionstore.StoredMessage, error) {
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

	return func(int, *llmwire.Response, []llmwire.ToolCall) error {
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
	return &llmwire.Response{Text: text}
}

func toolCallResponse(id, name string) *llmwire.Response {
	return &llmwire.Response{
		ToolCalls: []llmwire.ToolCall{{ID: id, Name: name, Arguments: []byte(`{}`)}},
	}
}

func TestRunLoopFinalTextResponseEndsRun(t *testing.T) {
	llmClient := &loopScriptLLM{responses: []*llmwire.Response{textResponse("all done")}}
	notifier := &loopNotifier{}

	agent := newTestAgent()
	agent.llmClient = llmClient
	agent.maxIterations = 5

	result, err := runLoop(t.Context(), agent, loopOptions{Notify: notifier.fn}, iterationGuard(5))

	require.NoError(t, err)
	assert.Equal(t, "all done", result.FinalResponse)
	assert.Equal(t, 1, result.Iterations)
	assert.Equal(t, []string{"all done"}, notifier.all())
}

func TestRunLoopDoesNotRepublishPreviousFinalBeforeAcceptingNextInput(t *testing.T) {
	agent := newTestAgent()
	agent.maxIterations = 5
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
	agent.maxIterations = 5
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
	agent.maxIterations = 5
	agent.boundary = &loopInputBoundary{
		agent: agent,
		input: &PendingInput{ID: 1, Content: "/status", ReceivedAt: time.Now()},
	}
	llmClient := &loopScriptLLM{responses: []*llmwire.Response{textResponse("must not run")}}
	agent.llmClient = llmClient
	notifier := &loopNotifier{}

	_, err := runLoop(t.Context(), agent, loopOptions{Notify: notifier.fn}, iterationGuard(5))

	require.NoError(t, err)
	assert.Zero(t, llmClient.calls)
	assert.Empty(t, agent.ms.getMessages())
	assert.Equal(t, 1, notifier.countWith("Session Status"))
}

func TestRunLoopStopsExactlyAtMaxIterations(t *testing.T) {
	llmClient := &loopScriptLLM{onCall: func(call int, _ []llmwire.Message) (*llmwire.Response, error) {
		return toolCallResponse(fmt.Sprintf("tc_%d", call), "read"), nil
	}}

	agent := newTestAgent(&stubTool{id: "read", result: "content"})
	agent.llmClient = llmClient
	agent.maxIterations = 3

	result, err := runLoop(t.Context(), agent, loopOptions{}, iterationGuard(10))

	require.Error(t, err)
	require.EqualError(t, err, "maximum iterations (3) reached")
	assert.Equal(t, 3, result.Iterations)
	assert.Equal(t, 3, llmClient.calls)
}

func TestRunLoopUnlimitedAgentFallsBackToHardCeiling(t *testing.T) {
	llmClient := &loopScriptLLM{onCall: func(call int, _ []llmwire.Message) (*llmwire.Response, error) {
		return toolCallResponse(fmt.Sprintf("tc_%d", call), "read"), nil
	}}

	agent := newTestAgent(&stubTool{id: "read", result: "content"})
	agent.llmClient = llmClient
	agent.maxIterations = 0

	result, err := runLoop(t.Context(), agent, loopOptions{}, iterationGuard(hardIterationCeiling+5))

	require.Error(t, err)
	require.EqualError(t, err, fmt.Sprintf("maximum iterations (%d) reached", hardIterationCeiling))
	assert.Equal(t, hardIterationCeiling, result.Iterations)
}

func TestRunLoopCancelledContextStopsBeforeFirstCall(t *testing.T) {
	llmClient := &loopScriptLLM{responses: []*llmwire.Response{textResponse("never")}}

	agent := newTestAgent()
	agent.llmClient = llmClient
	agent.maxIterations = 5

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
	agent.maxIterations = 5

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
	agent.maxIterations = 5

	_, err := runLoop(t.Context(), agent, loopOptions{Notify: notifier.fn}, iterationGuard(5))

	require.NoError(t, err)
	assert.Equal(t, []string{"done"}, notifier.all())
}

func TestRunLoopEmptyResponseLadder(t *testing.T) {
	llmClient := &loopScriptLLM{responses: []*llmwire.Response{{}}}
	notifier := &loopNotifier{}

	agent := newTestAgent()
	agent.llmClient = llmClient
	agent.maxIterations = 20

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
		if call == 3 {
			return toolCallResponse("tc_break", "read"), nil
		}

		return &llmwire.Response{}, nil
	}}

	agent := newTestAgent(&stubTool{id: "read", result: "content"})
	agent.llmClient = llmClient
	agent.maxIterations = 6

	result, err := runLoop(t.Context(), agent, loopOptions{}, iterationGuard(10))

	// The streak is broken at two, so the run exhausts its iterations instead of
	// bailing out on six empties.
	require.Error(t, err)
	assert.Equal(t, 6, result.Iterations)

	for _, msg := range agent.ms.getMessages() {
		assert.NotContains(t, msg.Content, "AUTOMATED WARNING")
	}
}

func TestRunLoopEmptyResponseNudgeRecordFailureAborts(t *testing.T) {
	store := &mockSessionStore{insertErr: errors.New("disk full"), insertFailAt: 2}
	llmClient := &loopScriptLLM{responses: []*llmwire.Response{{}}}

	agent := newTestAgent()
	agent.llmClient = llmClient
	agent.ms = newMessageStore(store, 1)
	agent.maxIterations = 20

	result, err := runLoop(t.Context(), agent, loopOptions{}, iterationGuard(20))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "record empty-response nudge")
	require.ErrorIs(t, result.Error, store.insertErr)
}

func TestRunLoopSuspendedToolEndsRunWithoutError(t *testing.T) {
	llmClient := &loopScriptLLM{responses: []*llmwire.Response{toolCallResponse("tc_1", "sleep")}}

	agent := newTestAgent(&stubTool{id: "sleep", err: tool.ErrSuspend})
	agent.llmClient = llmClient
	agent.maxIterations = 5

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
	agent.ms = newMessageStore(store, 1)
	agent.maxIterations = 5

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
	agent.maxIterations = 5
	agent.ms.setMessages(loopRounds(10, 40000))

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
	agent.maxIterations = 5
	agent.ms.setMessages(loopRounds(10, 40000))
	agent.RequestCompaction(4)

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
	agent.ms = newMessageStore(&loopReloadStore{replaceErr: errors.New("write conflict")}, 1)
	agent.ms.setMessages(loopRounds(10, 40000))
	agent.maxIterations = 5
	agent.RequestCompaction(4)

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
	agent.maxIterations = 5
	agent.ms.setMessages([]llmwire.Message{{Role: llmwire.RoleUser, Content: strings.Repeat("x", 400000)}})

	_, err := runLoop(t.Context(), agent, loopOptions{Notify: notifier.fn}, iterationGuard(5))
	require.NoError(t, err)

	assert.Equal(t, 1, notifier.countWith("🔄 Compacting context..."))
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
			agent.maxIterations = 5
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
	}{
		{name: "text response releases the model", response: textResponse("explaining myself"), want: false},
		{name: "tool call keeps the clamp on", response: toolCallResponse("tc_1", "read"), want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			llmClient := &loopScriptLLM{responses: []*llmwire.Response{tt.response}}

			agent := newTestAgent(&stubTool{id: "read", result: "a"})
			agent.llmClient = llmClient
			agent.maxIterations = 1
			agent.loopDetector.forceTextOnly = true

			_, err := runLoop(t.Context(), agent, loopOptions{}, iterationGuard(5))
			require.Error(t, err) // the single iteration is exhausted

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
	agent.maxIterations = 5

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
	agent.maxIterations = 5

	var (
		iterations []int
		seenCalls  [][]llmwire.ToolCall
	)

	cb := func(iteration int, resp *llmwire.Response, toolCalls []llmwire.ToolCall) error {
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
	agent.maxIterations = 5

	cbErr := errors.New("checkpoint failed")
	cb := func(int, *llmwire.Response, []llmwire.ToolCall) error { return cbErr }

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
	agent.ms = newMessageStore(store, 1)
	agent.maxIterations = 5

	result, err := runLoop(t.Context(), agent, loopOptions{}, iterationGuard(5))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "record assistant message")
	require.ErrorIs(t, result.Error, store.insertErr)
}

func TestRunLoopFinalizeRecoversLastAssistantText(t *testing.T) {
	llmClient := &loopScriptLLM{responses: []*llmwire.Response{textResponse("partial answer")}}
	notifier := &loopNotifier{}

	agent := newTestAgent()
	agent.llmClient = llmClient
	agent.maxIterations = 1

	result, err := runLoop(t.Context(), agent, loopOptions{Notify: notifier.fn}, iterationGuard(5))

	require.Error(t, err)
	require.EqualError(t, err, "maximum iterations (1) reached")
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
			llmClient := &loopScriptLLM{responses: []*llmwire.Response{textResponse("partial answer")}}
			notifier := &loopNotifier{err: tt.notifyErr}

			agent := newTestAgent()
			agent.llmClient = llmClient
			agent.maxIterations = 1

			core, logs := observer.New(zapcore.WarnLevel)
			ctx := logger.ToContext(t.Context(), zap.New(core))

			result, err := runLoop(ctx, agent, loopOptions{Notify: notifier.fn}, iterationGuard(5))

			require.Error(t, err) // the single iteration is exhausted
			assert.Equal(t, "partial answer", result.FinalResponse)
			assert.Equal(t, []string{"partial answer"}, notifier.all())
			assert.Len(t, logs.FilterMessage("notify_failed").All(), tt.wantLog)
		})
	}
}

func TestRunLoopFinalizeIgnoresBlankAssistantText(t *testing.T) {
	llmClient := &loopScriptLLM{responses: []*llmwire.Response{{
		Text:      "   ",
		ToolCalls: []llmwire.ToolCall{{ID: "tc_1", Name: "read", Arguments: []byte(`{}`)}},
	}}}
	notifier := &loopNotifier{}

	agent := newTestAgent(&stubTool{id: "read", result: "content"})
	agent.llmClient = llmClient
	agent.maxIterations = 1

	result, err := runLoop(t.Context(), agent, loopOptions{Notify: notifier.fn}, iterationGuard(5))

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
	agent.maxIterations = 5

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
			agent.ms = newMessageStore(&loopReloadStore{loadErr: tt.loadErr}, 1)
			agent.maxIterations = 5

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
			agent.maxIterations = 5

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
			agent.maxIterations = 5

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
