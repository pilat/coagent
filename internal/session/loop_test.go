package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/tool"
)

func TestShouldCompact(t *testing.T) {
	const window = 80000

	cutoff := compactionCutoff(window) // 68000

	tests := []struct {
		name     string
		tokens   int
		baseline *contextBaseline
		want     bool
	}{
		{"below cutoff, unmeasured", cutoff - 1000, nil, false},
		{"above cutoff, unmeasured", cutoff + 1000, nil, true},
		{"at cutoff exactly is not over", cutoff, nil, false},
		{
			"measurement outranks the estimate downward",
			cutoff + 1000,
			&contextBaseline{promptTokens: 1000, messageCount: 1},
			false,
		},
		{
			"measurement outranks the estimate upward",
			cutoff - 1000,
			&contextBaseline{promptTokens: cutoff + 1, messageCount: 1},
			true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			agent := newTestAgent()
			agent.prompt = newPromptBuilder("", "", "") // zero overhead: the cutoff cases are exact
			agent.ms.setMessages(buildMessagesWithTokens(tc.tokens))
			if tc.baseline != nil {
				agent.recordContextBaseline(
					tc.baseline.promptTokens,
					tc.baseline.messageCount,
					agent.modelGeneration(),
				)
			}

			assert.Equal(t, tc.want, agent.shouldCompact(window))
		})
	}
}

// The delta is counted over the tail alone: the measured prefix keeps its
// measured cost no matter what len/4 thinks of it.
func TestProjectContextSizeCountsOnlyTheTailAfterTheBaseline(t *testing.T) {
	msgs := append(buildMessagesWithTokens(50000), buildMessagesWithTokens(1000)...)
	base := &contextBaseline{promptTokens: 9000, messageCount: 1}

	assert.Equal(t, 10000, projectContextSize(msgs, base, 777))
	assert.Equal(t, 51777, projectContextSize(msgs, nil, 777), "no measurement: whole transcript plus overhead")
}

// A baseline that no longer indexes the transcript (compaction shrank it under
// the recorded position) must not be trusted into an inflated projection.
func TestProjectContextSizeDiscardsAStaleBaseline(t *testing.T) {
	msgs := buildMessagesWithTokens(1000)
	base := &contextBaseline{promptTokens: 90000, messageCount: 40}

	assert.Equal(t, 1100, projectContextSize(msgs, base, 100))
}

func TestCallLLMRecordsTheProviderBaseline(t *testing.T) {
	agent := newTestAgent()
	agent.maxIterations = 5
	agent.llmClient = &loopScriptLLM{responses: []*llmwire.Response{
		{Text: "done", Usage: &llmwire.MessageUsage{PromptTokens: 5000}},
	}}
	agent.ms.setMessages(buildMessagesWithTokens(1000))

	_, err := runLoop(t.Context(), agent, loopOptions{}, iterationGuard(5))
	require.NoError(t, err)

	base := agent.loadContextBaseline()
	require.NotNil(t, base)
	assert.Equal(t, 5000, base.promptTokens)
	assert.Equal(t, 1, base.messageCount, "the position is the transcript as sent, before the reply landed")

	size, estimated := agent.projectContextSize()
	assert.False(t, estimated)
	// The assistant reply ("done") is the tail the measurement did not cover.
	assert.Equal(t, 5000+estimateTokens(agent.ms.getMessages()[1:]), size)
}

func TestCallLLMLeavesTheProjectionEstimatedWithoutUsage(t *testing.T) {
	agent := newTestAgent()
	agent.maxIterations = 5
	agent.llmClient = &loopScriptLLM{responses: []*llmwire.Response{
		{Text: "done", Usage: &llmwire.MessageUsage{PromptTokens: 0}},
	}}
	agent.ms.setMessages(buildMessagesWithTokens(1000))

	_, err := runLoop(t.Context(), agent, loopOptions{}, iterationGuard(5))
	require.NoError(t, err)

	assert.Nil(t, agent.loadContextBaseline(), "a provider that reports zero has measured nothing")

	_, estimated := agent.projectContextSize()
	assert.True(t, estimated)
}

// TestEstimateTokensCountsToolCallArguments pins that write/edit/apply_patch file
// bodies (carried in tool-call Arguments) are part of the trigger estimate.
func TestEstimateTokensCountsToolCallArguments(t *testing.T) {
	args := []byte(strings.Repeat("x", 4000)) // ~1000 est tokens
	msgs := []llmwire.Message{{
		Role:      llmwire.RoleAssistant,
		ToolCalls: []llmwire.ToolCall{{ID: "c1", Name: "write", Arguments: args}},
	}}

	assert.Equal(t, 1000, estimateTokens(msgs))
}

func TestFormatToolResult_SizeDiffersByContextWindow(t *testing.T) {
	bigOutput := strings.Repeat("x", 100000)
	result := &tool.Result{Output: bigOutput}

	smallWindowResult := formatToolResult(result, 15000)
	largeWindowResult := formatToolResult(result, 200000)

	assert.Less(t, len(smallWindowResult), len(largeWindowResult))
}

func TestFormatToolResult_PreservesPresentationContract(t *testing.T) {
	tests := []struct {
		name   string
		result *tool.Result
		want   string
	}{
		{
			name:   "plain output",
			result: &tool.Result{Output: "body"},
			want:   "body",
		},
		{
			name:   "title",
			result: &tool.Result{Title: "Read file", Output: "body"},
			want:   "[Read file]\nbody",
		},
		{
			name: "self-reported truncation",
			result: &tool.Result{
				Output:   "body",
				Metadata: map[string]any{"truncated": true},
			},
			want: "body\n(output truncated: 4 bytes total)",
		},
		{
			name: "false truncation metadata",
			result: &tool.Result{
				Output:   "body",
				Metadata: map[string]any{"truncated": false},
			},
			want: "body",
		},
		{
			name: "malformed truncation metadata",
			result: &tool.Result{
				Output:   "body",
				Metadata: map[string]any{"truncated": "true"},
			},
			want: "body",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, formatToolResult(tt.result, 200000))
		})
	}
}

func TestPrependLoopWarning_ReportsExactWindowDiversity(t *testing.T) {
	agent := newTestAgent()
	agent.loopDetector.window = []toolRecord{
		{name: "read", resultHash: 1},
		{name: "read", resultHash: 1},
		{name: "grep", resultHash: 2},
		{name: "grep", resultHash: 2},
	}

	want := fmt.Sprintf(loopWarningTemplate, 50, 4, 2) + "\n\nbody"
	assert.Equal(t, want, prependLoopWarning(t.Context(), agent, actionWarn, "grep", "body"))
}

func TestPrependLoopWarning_HandlesEmptyWindow(t *testing.T) {
	agent := newTestAgent()

	want := fmt.Sprintf(loopWarningTemplate, 0, 0, 0) + "\n\nbody"
	assert.Equal(t, want, prependLoopWarning(t.Context(), agent, actionWarn, "read", "body"))
}

func TestPrependLoopWarning_ReportsExactFailureStreak(t *testing.T) {
	agent := newTestAgent()
	agent.loopDetector.window = []toolRecord{
		{name: "edit", failed: true},
		{name: "edit", failed: true},
		{name: "edit", failed: true},
	}

	want := fmt.Sprintf(loopFailureWarningTemplate, "edit", 3) + "\n\nbody"
	assert.Equal(t, want, prependLoopWarning(t.Context(), agent, actionWarnFailure, "edit", "body"))
}

func TestExecuteToolCall_RejectsNilResult(t *testing.T) {
	registry := tool.NewRegistry()
	registry.Register(&nilResultTool{})

	_, _, err := executeToolCall(t.Context(), registry, llmwire.ToolCall{
		ID:        "call-1",
		Name:      "nil-result",
		Arguments: json.RawMessage(`{}`),
	}, 200000)

	require.EqualError(t, err, "execute tool nil-result: tool returned nil result")
}

type nilResultTool struct{}

func (*nilResultTool) ID() string                  { return "nil-result" }
func (*nilResultTool) Description() string         { return "returns an invalid nil result" }
func (*nilResultTool) Parameters() json.RawMessage { return json.RawMessage(`{}`) }
func (*nilResultTool) Execute(context.Context, json.RawMessage) (*tool.Result, error) {
	return nil, nil
}

func buildMessagesWithTokens(tokens int) []llmwire.Message {
	totalChars := tokens * 4
	content := strings.Repeat("a", totalChars)
	return []llmwire.Message{{Role: llmwire.RoleUser, Content: content}}
}

func TestLastAssistantState(t *testing.T) {
	tests := []struct {
		name     string
		messages []llmwire.Message
		want     *assistantState
	}{
		{
			name:     "empty messages",
			messages: nil,
			want:     nil,
		},
		{
			name: "last message is user",
			messages: []llmwire.Message{
				{Role: llmwire.RoleUser, Content: "hello"},
			},
			want: nil,
		},
		{
			name: "last message is tool (all resolved)",
			messages: []llmwire.Message{
				{Role: llmwire.RoleUser, Content: "do stuff"},
				{Role: llmwire.RoleAssistant, Content: "", ToolCalls: []llmwire.ToolCall{
					{ID: "tc1", Name: "read", Arguments: []byte(`{}`)},
				}},
				{Role: llmwire.RoleTool, ToolCallID: "tc1", Content: "file content"},
			},
			want: nil,
		},
		{
			name: "assistant with text only",
			messages: []llmwire.Message{
				{Role: llmwire.RoleUser, Content: "hello"},
				{Role: llmwire.RoleAssistant, Content: "Here is my response"},
			},
			want: &assistantState{HasText: true, Text: "Here is my response"},
		},
		{
			name: "assistant empty (no text no tools)",
			messages: []llmwire.Message{
				{Role: llmwire.RoleUser, Content: "hello"},
				{Role: llmwire.RoleAssistant, Content: ""},
			},
			want: &assistantState{},
		},
		{
			name: "assistant with all pending tools (crash before execution)",
			messages: []llmwire.Message{
				{Role: llmwire.RoleUser, Content: "do stuff"},
				{Role: llmwire.RoleAssistant, Content: "Let me help", ToolCalls: []llmwire.ToolCall{
					{ID: "tc1", Name: "read", Arguments: []byte(`{"path":"a.go"}`)},
					{ID: "tc2", Name: "grep", Arguments: []byte(`{"pattern":"foo"}`)},
				}},
			},
			want: &assistantState{
				HasPendingTools: true,
				PendingTools: []llmwire.ToolCall{
					{ID: "tc1", Name: "read", Arguments: []byte(`{"path":"a.go"}`)},
					{ID: "tc2", Name: "grep", Arguments: []byte(`{"pattern":"foo"}`)},
				},
				HasText: true,
				Text:    "Let me help",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := lastAssistantState(tt.messages)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestCountUniqueOutcomes(t *testing.T) {
	window := []toolRecord{
		{name: "read", argsHash: 1, resultHash: 100},
		{name: "read", argsHash: 2, resultHash: 100},
		{name: "grep", argsHash: 3, resultHash: 200},
		{name: "grep", argsHash: 4, resultHash: 200},
		{name: "edit", argsHash: 5, resultHash: 300},
	}

	assert.Equal(t, 3, countUniqueOutcomes(window))
}

// stubTool is a minimal tool implementation for integration tests.
type stubTool struct {
	id     string
	result string
	err    error
}

func (s *stubTool) ID() string                  { return s.id }
func (s *stubTool) Description() string         { return "stub" }
func (s *stubTool) Parameters() json.RawMessage { return json.RawMessage(`{}`) }
func (s *stubTool) Execute(_ context.Context, _ json.RawMessage) (*tool.Result, error) {
	if s.err != nil {
		return nil, s.err
	}

	return &tool.Result{Output: s.result}, nil
}

// newTestAgent creates a minimal svc with the given tools registered.
func newTestAgent(tools ...tool.Tool) *svc {
	reg := tool.NewRegistry()
	for _, t := range tools {
		reg.Register(t)
	}
	return &svc{
		llmClient:    &mockLLMRunOnce{response: &llmwire.Response{Text: "ok"}},
		ms:           newMessageStore(nil, 0),
		loopDetector: newLoopDetector(),
		registry:     reg,
		prompt:       newPromptBuilder("test", "", ""),
	}
}

func TestExecuteToolCalls_WarnOnLowDiversity(t *testing.T) {
	agent := newTestAgent(&stubTool{id: "edit", result: "old_string not found"})

	tc := llmwire.ToolCall{Name: "edit", Arguments: []byte(`{"old":"a","new":"b"}`)}
	for i := range loopDetectorConsecutiveWarn {
		tc.ID = fmt.Sprintf("tc_%d", i)
		require.NoError(t, executeToolCalls(context.Background(), agent, []llmwire.ToolCall{tc}))
	}

	found := false
	for _, msg := range agent.ms.getMessages() {
		if msg.Role == llmwire.RoleTool && strings.Contains(msg.Content, "[LOOP WARNING:") {
			found = true
			break
		}
	}
	assert.True(t, found, "some tool result should contain [LOOP WARNING:")
}

func TestExecuteToolCalls_BlockAfterWarnIgnored(t *testing.T) {
	agent := newTestAgent(&stubTool{id: "edit", result: "old_string not found"})

	tc := llmwire.ToolCall{Name: "edit", Arguments: []byte(`{"old":"a","new":"b"}`)}

	for i := range loopDetectorConsecutiveWarn {
		tc.ID = fmt.Sprintf("tc_%d", i)
		require.NoError(t, executeToolCalls(context.Background(), agent, []llmwire.ToolCall{tc}))
	}
	require.True(t, agent.loopDetector.warnActive)

	tc.ID = "tc_block"
	require.NoError(t, executeToolCalls(context.Background(), agent, []llmwire.ToolCall{tc}))

	msgs := agent.ms.getMessages()
	lastToolMsg := msgs[len(msgs)-1]
	assert.Contains(t, lastToolMsg.Content, "[BLOCKED:", "should get block message after ignoring warn")
}

func TestExecuteToolCalls_NoWarningOnDiverseCalls(t *testing.T) {
	tools := make([]tool.Tool, 0, 20)
	for i := range 20 {
		tools = append(tools, &stubTool{
			id:     fmt.Sprintf("tool_%d", i),
			result: fmt.Sprintf("result_%d", i),
		})
	}
	agent := newTestAgent(tools...)

	for i := range 20 {
		tc := llmwire.ToolCall{
			ID:        fmt.Sprintf("tc_%d", i),
			Name:      fmt.Sprintf("tool_%d", i),
			Arguments: fmt.Appendf(nil, `{"key":"%d"}`, i),
		}
		require.NoError(t, executeToolCalls(context.Background(), agent, []llmwire.ToolCall{tc}))
	}

	for _, msg := range agent.ms.getMessages() {
		if msg.Role == llmwire.RoleTool {
			assert.NotContains(t, msg.Content, "[LOOP WARNING:")
			assert.NotContains(t, msg.Content, "[BLOCKED:")
		}
	}
}

func TestExecuteToolCalls_ParallelDedupInRound(t *testing.T) {
	agent := newTestAgent(&stubTool{id: "read", result: "content"})

	tcs := []llmwire.ToolCall{
		{ID: "tc_1", Name: "read", Arguments: []byte(`{"path":"a.go"}`)},
		{ID: "tc_2", Name: "read", Arguments: []byte(`{"path":"a.go"}`)},
	}
	require.NoError(t, executeToolCalls(context.Background(), agent, tcs))

	assert.Len(t, agent.loopDetector.window, 1)
}

func TestExecuteToolCalls_RejectsSleepAlongsideTaskBeforeSideEffect(t *testing.T) {
	taskTool := &countingTool{id: tool.IDTask}
	sleepTool := &countingTool{id: tool.IDSleep}
	agent := newTestAgent(taskTool, sleepTool)

	require.NoError(t, executeToolCalls(t.Context(), agent, []llmwire.ToolCall{
		{ID: "task-1", Name: tool.IDTask, Arguments: []byte(`{}`)},
		{ID: "sleep-1", Name: tool.IDSleep, Arguments: []byte(`{"duration":"10s"}`)},
	}))

	assert.Equal(t, int64(1), taskTool.runs.Load())
	assert.Zero(t, sleepTool.runs.Load(), "sleep must not stage a competing wake-up")
	messages := agent.ms.getMessages()
	require.Len(t, messages, 2)
	assert.Contains(t, messages[1].Content, "subagent completion wakes the session automatically")
}

func TestExecuteToolCalls_RejectsSleepAlongsideSubagentFollowUpBeforeSideEffect(t *testing.T) {
	followUpTool := &countingTool{id: tool.IDSendToSubagent}
	sleepTool := &countingTool{id: tool.IDSleep}
	agent := newTestAgent(followUpTool, sleepTool)

	require.NoError(t, executeToolCalls(t.Context(), agent, []llmwire.ToolCall{
		{ID: "follow-up-1", Name: tool.IDSendToSubagent, Arguments: []byte(`{"id":42,"message":"more"}`)},
		{ID: "sleep-1", Name: tool.IDSleep, Arguments: []byte(`{"duration":"10s"}`)},
	}))

	assert.Equal(t, int64(1), followUpTool.runs.Load())
	assert.Zero(t, sleepTool.runs.Load(), "sleep must not race durable follow-up acceptance")
	messages := agent.ms.getMessages()
	require.Len(t, messages, 2)
	assert.Contains(t, messages[1].Content, "subagent completion wakes the session automatically")
}

func TestExecuteToolCalls_RejectedSleepDoesNotSkipLaterIndependentTool(t *testing.T) {
	followUpTool := &countingTool{id: tool.IDSendToSubagent}
	sleepTool := &countingTool{id: tool.IDSleep}
	readTool := &countingTool{id: "read"}
	agent := newTestAgent(followUpTool, sleepTool, readTool)

	require.NoError(t, executeToolCalls(t.Context(), agent, []llmwire.ToolCall{
		{ID: "follow-up-1", Name: tool.IDSendToSubagent, Arguments: []byte(`{"id":42,"message":"more"}`)},
		{ID: "sleep-1", Name: tool.IDSleep, Arguments: []byte(`{"duration":"10s"}`)},
		{ID: "read-1", Name: "read", Arguments: []byte(`{"path":"next.go"}`)},
	}))

	assert.Equal(t, int64(1), followUpTool.runs.Load())
	assert.Zero(t, sleepTool.runs.Load())
	assert.Equal(t, int64(1), readTool.runs.Load(),
		"rejecting one conflicting call must not truncate the parallel tool batch")
	messages := agent.ms.getMessages()
	require.Len(t, messages, 3)
	assert.Contains(t, messages[1].Content, "subagent completion wakes the session automatically")
	assert.Equal(t, "ran", messages[2].Content)
}

func TestExecuteToolCalls_ForceTextOnly(t *testing.T) {
	agent := newTestAgent(&stubTool{id: "edit", result: "error"})

	tc := llmwire.ToolCall{Name: "edit", Arguments: []byte(`{}`)}

	for i := range loopDetectorMinFill {
		tc.ID = fmt.Sprintf("tc_%d", i)
		require.NoError(t, executeToolCalls(context.Background(), agent, []llmwire.ToolCall{tc}))
	}
	for i := range loopDetectorMaxBlocks + 2 {
		tc.ID = fmt.Sprintf("tc_esc_%d", i)
		require.NoError(t, executeToolCalls(context.Background(), agent, []llmwire.ToolCall{tc}))
	}

	assert.True(t, agent.loopDetector.forceTextOnly)

	agent.loopDetector.clearForceTextOnly()
	assert.False(t, agent.loopDetector.forceTextOnly)
	assert.False(t, agent.loopDetector.blocked)
}
