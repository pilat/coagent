package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
)

func newMockSvc(t *testing.T, messages []llmwire.Message, agentsMD string) *svc {
	t.Helper()
	_, store, sessionID := newFinalOutputStore(t)
	ms := newMessageStore(store, sessionID, store)
	if messages != nil {
		ms.setMessages(messages)
	}
	s := &svc{
		rootID:    sessionID,
		id:        sessionID,
		store:     store,
		agentType: registry.AgentTypeBuild,
		todoStore: todo.New(),
		agentsMD:  agentsMD,
		ms:        ms,

		models:   newTestModelRuntime(&mockLLMClient{}, store, sessionID),
		prompt:   newPromptBuilder("test", ""),
		registry: tool.NewRegistry(),
	}
	s.turns = newToolTurns(s.registry, s.models, s.ms, testProgressBoundary(s.boundary))
	return s
}

func setRunTestModel(s *svc, client *loopScriptLLM) {
	s.models = newTestModelRuntime(client, s.store, s.id)
	s.turns = newToolTurns(s.registry, s.models, s.ms, testProgressBoundary(s.boundary))
}

func TestRun_FreshSessionInjectsAgentsMDAndPrompt(t *testing.T) {
	s := newMockSvc(t, nil, "Be concise.")
	setRunTestModel(s, &loopScriptLLM{responses: []*llmwire.Response{textResponse("hello"), textResponse("confirmed")}})
	prepareDirectRun(t, s, "write tests")
	result, err := s.Run(t.Context(), "write tests")
	require.NoError(t, err)
	assert.Equal(t, "hello", result)
	msgs := s.ms.getMessages()
	require.GreaterOrEqual(t, len(msgs), 2)
	assert.Equal(t, llmwire.RoleUser, msgs[0].Role)
	assert.True(t, strings.HasPrefix(msgs[0].Content, "User preferences from AGENTS.md"))
	assert.Equal(t, llmwire.RoleUser, msgs[1].Role)
	assert.Contains(t, msgs[1].Content, "write tests")
	assert.Regexp(t, `^\[\w+ \d{4}-\d{2}-\d{2} \d{2}:\d{2} \w+ [+-]\d{2}:\d{2}\]`, msgs[1].Content)
}

func TestRun_FreshSessionNoAgentsMD(t *testing.T) {
	s := newMockSvc(t, nil, "")
	setRunTestModel(s, &loopScriptLLM{responses: []*llmwire.Response{textResponse("ok"), textResponse("confirmed")}})
	prepareDirectRun(t, s, "hello")
	_, err := s.Run(t.Context(), "hello")
	require.NoError(t, err)
	msgs := s.ms.getMessages()
	require.GreaterOrEqual(t, len(msgs), 1)
	assert.Contains(t, msgs[0].Content, "hello")
	assert.Regexp(t, `^\[\w+ \d{4}-\d{2}-\d{2} \d{2}:\d{2} \w+ [+-]\d{2}:\d{2}\]`, msgs[0].Content)
}

func TestRun_FreshSessionEmptyPromptGetsDefault(t *testing.T) {
	s := newMockSvc(t, nil, "")
	setRunTestModel(s, &loopScriptLLM{responses: []*llmwire.Response{textResponse("hi"), textResponse("confirmed")}})
	prepareDirectRun(t, s, "")
	_, err := s.Run(t.Context(), "")
	require.NoError(t, err)
	msgs := s.ms.getMessages()
	require.GreaterOrEqual(t, len(msgs), 1)
	assert.Contains(t, msgs[0].Content, "hasn't provided a task yet")
}

func TestRun_ResumedSessionAddsOnlyNewPrompt(t *testing.T) {
	s := newMockSvc(t, []llmwire.Message{
		{Role: llmwire.RoleUser, Content: "old prompt"},
		{Role: llmwire.RoleAssistant, Content: "old reply"},
	}, "ignored on resume")
	setRunTestModel(
		s,
		&loopScriptLLM{responses: []*llmwire.Response{textResponse("continued"), textResponse("confirmed")}},
	)
	prepareDirectRun(t, s, "continue please")
	result, err := s.Run(t.Context(), "continue please")
	require.NoError(t, err)
	assert.Equal(t, "continued", result)
	msgs := s.ms.getMessages()
	require.GreaterOrEqual(t, len(msgs), 3)
	assert.Equal(t, "old prompt", msgs[0].Content)
	assert.Equal(t, "old reply", msgs[1].Content)
	assert.Contains(t, msgs[2].Content, "continue please")
}

func TestRun_ResumedSessionEmptyPromptNoNewMessage(t *testing.T) {
	s := newMockSvc(t, []llmwire.Message{
		{Role: llmwire.RoleUser, Content: "previous task"},
		{Role: llmwire.RoleAssistant, Content: "working on it"},
	}, "")
	setRunTestModel(s, &loopScriptLLM{responses: []*llmwire.Response{textResponse("done"), textResponse("confirmed")}})
	prepareDirectRun(t, s, "")
	_, err := s.Run(t.Context(), "")
	require.NoError(t, err)
	assert.Len(t, s.ms.getMessages(), 2)
}

func TestRun_PersistStateCalledPerIteration(t *testing.T) {
	s := newMockSvc(t, nil, "")
	setRunTestModel(
		s,
		&loopScriptLLM{responses: []*llmwire.Response{textResponse("answer"), textResponse("confirmed")}},
	)
	prepareDurableLoop(t, s)
	_, err := s.Run(t.Context(), "multi-step")
	require.NoError(t, err)
	store := s.store.(sessionstore.Store)
	record, err := store.GetSession(t.Context(), s.id)
	require.NoError(t, err)
	assert.Equal(t, 2, record.Iteration)
	assert.Equal(t, sessionstore.SessionStatusCompleted, record.Status)
}

func TestRunDaemon_PreservesNewToolSuspensionAcrossSessionBoundary(t *testing.T) {
	s := newMockSvc(t, nil, "")
	s.registry.Register(&stubTool{id: tool.IDSleep, err: tool.ErrSuspend})
	setRunTestModel(s, &loopScriptLLM{responses: []*llmwire.Response{toolCallResponse("sleep-call", tool.IDSleep)}})
	prepareDurableLoop(t, s)
	result, err := s.RunDaemon(t.Context(), nil, nil)
	require.NoError(t, err)
	assert.True(t, result.Suspended,
		"RunDaemon must return the loop result, not reconstruct suspension from pre-run ledgers")
	require.NoError(t, s.ResolveInterruptedCalls(t.Context(),
		[]PendingToolCall{{ID: "sleep-call", Name: tool.IDSleep}}, "woke"))
	s.models = newTestModelRuntime(
		&loopScriptLLM{responses: []*llmwire.Response{textResponse("done"), textResponse("confirmed")}},
		s.store,
		s.id,
	)
	s.turns = newToolTurns(s.registry, s.models, s.ms, testProgressBoundary(s.boundary))
	s.contexts = newCheckpointOwner(
		s.ms, s.models, s.prompt, s.turns, s.transcript(),
		s.dispositions, s.budgetGate, s.outputStore, s.boundary,
		&s.stamper, nil, nil,
		checkpointOptions{id: s.id, outputEnabled: s.outputEnabled, agentsMD: s.agentsMD},
	)

	result, err = s.RunDaemon(t.Context(), nil, nil)
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
