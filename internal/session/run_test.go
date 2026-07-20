package session

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
)

// newMockSvc creates a minimal *svc with a mock LLM for run tests.
// The mock LLM returns a single text response and exits.
func newMockSvc(t *testing.T, messages []llmwire.Message, agentsMD string) *svc {
	t.Helper()
	ms := newMessageStore(nil, 0)
	if messages != nil {
		ms.setMessages(messages)
	}
	return &svc{
		rootID:       1,
		id:           1,
		agentType:    registry.AgentTypeBuild,
		todoStore:    todo.New(),
		agentsMD:     agentsMD,
		ms:           ms,
		loopDetector: newLoopDetector(),
		llmClient:    &mockLLMClient{},
		prompt:       newPromptBuilder("test", "", ""),
		registry:     tool.NewRegistry(),
	}
}

func TestRun_FreshSessionInjectsAgentsMDAndPrompt(t *testing.T) {
	mockLLM := &mockLLMRunOnce{response: &llmwire.Response{Text: "hello"}}
	s := newMockSvc(t, nil, "Be concise.")
	s.llmClient = mockLLM

	result, err := s.Run(context.Background(), "write tests")
	require.NoError(t, err)
	assert.Equal(t, "hello", result)

	msgs := s.ms.getMessages()
	// AGENTS.md + prompt + assistant response = 3 messages minimum
	require.GreaterOrEqual(t, len(msgs), 2)
	assert.Equal(t, llmwire.RoleUser, msgs[0].Role)
	assert.True(t, strings.HasPrefix(msgs[0].Content, "User preferences from AGENTS.md"))
	assert.Equal(t, llmwire.RoleUser, msgs[1].Role)
	assert.Contains(t, msgs[1].Content, "write tests")
	assert.Regexp(t, `^\[\w+ \d{4}-\d{2}-\d{2} \d{2}:\d{2}\]`, msgs[1].Content)
}

func TestRun_FreshSessionNoAgentsMD(t *testing.T) {
	mockLLM := &mockLLMRunOnce{response: &llmwire.Response{Text: "ok"}}
	s := newMockSvc(t, nil, "")
	s.llmClient = mockLLM

	_, err := s.Run(context.Background(), "hello")
	require.NoError(t, err)

	msgs := s.ms.getMessages()
	require.GreaterOrEqual(t, len(msgs), 1)
	assert.Contains(t, msgs[0].Content, "hello")
	assert.Regexp(t, `^\[\w+ \d{4}-\d{2}-\d{2} \d{2}:\d{2}\]`, msgs[0].Content)
}

func TestRun_FreshSessionEmptyPromptGetsDefault(t *testing.T) {
	mockLLM := &mockLLMRunOnce{response: &llmwire.Response{Text: "hi"}}
	s := newMockSvc(t, nil, "")
	s.llmClient = mockLLM

	_, err := s.Run(context.Background(), "")
	require.NoError(t, err)

	msgs := s.ms.getMessages()
	require.GreaterOrEqual(t, len(msgs), 1)
	assert.Contains(t, msgs[0].Content, "hasn't provided a task yet")
}

func TestRun_ResumedSessionAddsOnlyNewPrompt(t *testing.T) {
	mockLLM := &mockLLMRunOnce{response: &llmwire.Response{Text: "continued"}}
	s := newMockSvc(t, []llmwire.Message{
		{Role: llmwire.RoleUser, Content: "old prompt"},
		{Role: llmwire.RoleAssistant, Content: "old reply"},
	}, "ignored on resume")
	s.llmClient = mockLLM

	result, err := s.Run(context.Background(), "continue please")
	require.NoError(t, err)
	assert.Equal(t, "continued", result)

	msgs := s.ms.getMessages()
	require.GreaterOrEqual(t, len(msgs), 3)
	assert.Equal(t, "old prompt", msgs[0].Content)
	assert.Equal(t, "old reply", msgs[1].Content)
	assert.Contains(t, msgs[2].Content, "continue please")
}

func TestRun_ResumedSessionEmptyPromptNoNewMessage(t *testing.T) {
	mockLLM := &mockLLMRunOnce{response: &llmwire.Response{Text: "done"}}
	s := newMockSvc(t, []llmwire.Message{
		{Role: llmwire.RoleUser, Content: "previous task"},
		{Role: llmwire.RoleAssistant, Content: "working on it"},
	}, "")
	s.llmClient = mockLLM
	s.rootID = 5
	s.id = 5

	// The loop will see "working on it" as final text and return immediately.
	_, err := s.Run(context.Background(), "")
	require.NoError(t, err)

	msgs := s.ms.getMessages()
	require.Len(t, msgs, 2) // No new message added
}

func TestRun_PersistStateCalledPerIteration(t *testing.T) {
	updater := &mockSessionStore{}
	mockLLM := &mockLLMSequence{
		responses: []*llmwire.Response{
			{Text: "step1"},
			{Text: "step2"},
			{Text: "done"},
		},
	}

	s := newMockSvc(t, nil, "")
	s.llmClient = mockLLM
	s.rootID = 6
	s.id = 6
	s.store = updater
	s.maxIterations = 3

	_, err := s.Run(context.Background(), "multi-step")
	require.NoError(t, err)

	// At least some iteration + final persist calls
	assert.Positive(t, updater.iterationCalls)
}

func TestRunDaemon_PreservesNewToolSuspensionAcrossSessionBoundary(t *testing.T) {
	s := newMockSvc(t, nil, "")
	s.registry.Register(&stubTool{id: tool.IDSleep, err: tool.ErrSuspend})
	s.llmClient = &loopScriptLLM{responses: []*llmwire.Response{
		toolCallResponse("sleep-call", tool.IDSleep),
	}}
	s.maxIterations = 5

	result, err := s.RunDaemon(t.Context(), nil)
	require.NoError(t, err)
	assert.True(t, result.Suspended,
		"RunDaemon must return the loop result, not reconstruct suspension from pre-run ledgers")

	// A later settled run on the same service must not inherit the previous
	// in-memory suspend flag.
	s.ms.setMessages([]llmwire.Message{
		{Role: llmwire.RoleUser, Content: "settled"},
		{Role: llmwire.RoleAssistant, Content: "done"},
	})
	s.llmClient = &mockLLMRunOnce{response: &llmwire.Response{Text: "unused"}}

	result, err = s.RunDaemon(t.Context(), nil)
	require.NoError(t, err)
	assert.False(t, result.Suspended)
}

func TestLastAssistantTextOnly(t *testing.T) {
	tests := []struct {
		name     string
		messages []llmwire.Message
		want     string
	}{
		{
			name:     "empty history",
			messages: []llmwire.Message{},
			want:     "",
		},
		{
			name: "last message is assistant with no tool calls",
			messages: []llmwire.Message{
				{Role: llmwire.RoleUser, Content: "hello"},
				{Role: llmwire.RoleAssistant, Content: "how can I help?"},
			},
			want: "how can I help?",
		},
		{
			name: "last message is assistant WITH tool calls",
			messages: []llmwire.Message{
				{Role: llmwire.RoleUser, Content: "run something"},
				{
					Role:      llmwire.RoleAssistant,
					Content:   "sure",
					ToolCalls: []llmwire.ToolCall{{ID: "tc1", Name: "bash"}},
				},
			},
			want: "",
		},
		{
			name: "last message is user",
			messages: []llmwire.Message{
				{Role: llmwire.RoleAssistant, Content: "previous reply"},
				{Role: llmwire.RoleUser, Content: "follow-up question"},
			},
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := lastAssistantTextOnly(tc.messages)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestLastUserMessage(t *testing.T) {
	tests := []struct {
		name     string
		messages []llmwire.Message
		want     string
	}{
		{
			name:     "empty history",
			messages: []llmwire.Message{},
			want:     "",
		},
		{
			name: "last message is user",
			messages: []llmwire.Message{
				{Role: llmwire.RoleAssistant, Content: "previous reply"},
				{Role: llmwire.RoleUser, Content: "my latest question"},
			},
			want: "my latest question",
		},
		{
			name: "last message is assistant",
			messages: []llmwire.Message{
				{Role: llmwire.RoleUser, Content: "hello"},
				{Role: llmwire.RoleAssistant, Content: "hi there"},
			},
			want: "hello",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := lastUserMessage(tc.messages)
			assert.Equal(t, tc.want, got)
		})
	}
}
