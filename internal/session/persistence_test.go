package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/tool/builtin"
	"github.com/pilat/coagent/internal/transcript"
)

func TestRun_FailFastOnPersistError(t *testing.T) {
	mockStore := &mockSessionStore{failCall: 1}
	mockLLM := &mockLLMRunOnce{response: &llmwire.Response{Text: "done"}}

	s := &svc{
		rootID:        1,
		id:            1,
		agentType:     registry.AgentTypeBuild,
		llmClient:     mockLLM,
		todoStore:     todo.New(),
		store:         mockStore,
		ms:            newMessageStore(nil, 0),
		loopDetector:  newLoopDetector(),
		prompt:        newPromptBuilder("test", "", ""),
		maxIterations: 10,
		registry:      tool.NewRegistry(),
	}

	_, err := s.Run(context.Background(), "do something")
	if err == nil {
		t.Fatal("expected fail-fast error, got nil")
	}
	if got := err.Error(); got == "" || !strings.Contains(got, "persist") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewWithOptions_ResumeFromDB(t *testing.T) {
	resumeMessages := []llmwire.Message{
		{Role: llmwire.RoleUser, Content: "initial"},
		{Role: llmwire.RoleAssistant, Content: "ack"},
	}
	resumeTodos := []*todo.Item{
		{ID: "todo-1", Content: "resume task", Status: todo.StatusPending},
	}

	reg := tool.NewRegistry()
	reg.Register(builtin.NewLsTool("/tmp/project"))

	p := params{
		Config:    &config.Config{WorkDir: "/tmp/project", Model: "test-model"},
		LLMClient: &mockLLMClient{},
		TodoStore: todo.New(),
		Loader:    loader.New(),
		Registry:  reg,
	}

	sessionSvc, err := newWithOptions(context.Background(), p, options{
		ID:              1,
		ResumeMessages:  resumeMessages,
		ResumeIteration: 5,
		ResumeTodoItems: resumeTodos,
	})
	if err != nil {
		t.Fatalf("newWithOptions failed: %v", err)
	}

	s := sessionSvc.(*svc)
	s.newLLMWithModel = func(_ *config.Config, _ string) (llm.Client, error) {
		return &mockLLMClient{}, nil
	}

	if got, want := len(s.ms.getMessages()), 2; got != want {
		t.Fatalf("main messages not hydrated: got %d, want %d", got, want)
	}
	if got, want := s.iterationOffset, 5; got != want {
		t.Fatalf("iteration offset mismatch: got %d, want %d", got, want)
	}
	items := s.todoStore.List()
	if got, want := len(items), 1; got != want {
		t.Fatalf("todo items count mismatch: got %d, want %d", got, want)
	}
	if got, want := items[0].Content, "resume task"; got != want {
		t.Fatalf("todo item content mismatch: got %q, want %q", got, want)
	}
}

// mockSessionStore implements only the live-session persistence capability.
type mockSessionStore struct {
	failCall       int
	iterationCalls int

	// InsertMessage: hands out monotonic ids; returns insertErr on the
	// insertFailAt-th call (0 = every call).
	nextMsgID    int64
	insertCalls  int
	insertFailAt int
	insertErr    error

	// InsertToolNotificationPair: knob plus what the last call was handed.
	pairErr        error
	lastPairAsst   *transcript.Message
	lastPairResult *transcript.Message
}

var _ sessionstore.RuntimeStore = (*mockSessionStore)(nil)

func (m *mockSessionStore) InsertMessage(_ context.Context, _ int64, _ *transcript.Message) (int64, error) {
	m.insertCalls++
	if m.insertErr != nil && (m.insertFailAt == 0 || m.insertCalls == m.insertFailAt) {
		return 0, m.insertErr
	}

	m.nextMsgID++

	return m.nextMsgID, nil
}

func (m *mockSessionStore) MarkCompacted(_ context.Context, _ []int64) error { return nil }
func (m *mockSessionStore) MarkCleared(_ context.Context, _ []int64) error   { return nil }
func (m *mockSessionStore) ReplaceCompactedMessages(
	_ context.Context,
	_ int64,
	_ []int64,
	entries []sessionstore.CompactionEntry,
) ([]int64, error) {
	return make([]int64, len(entries)), nil
}

func (m *mockSessionStore) LoadActiveMessages(_ context.Context, _ int64) ([]*transcript.Message, error) {
	return nil, nil
}

func (m *mockSessionStore) UpdateSessionIteration(
	_ context.Context,
	_ int64,
	_ int,
	_ sessionstore.SessionStatus,
) error {
	m.iterationCalls++
	if m.failCall > 0 && m.iterationCalls == m.failCall {
		return fmt.Errorf("forced store failure")
	}
	return nil
}

func (m *mockSessionStore) UpdateSessionTodoItems(_ context.Context, _ int64, _ json.RawMessage) error {
	return nil
}

func (m *mockSessionStore) UpdateSessionCompactionBrief(_ context.Context, _ int64, _ string) error {
	return nil
}

func (m *mockSessionStore) GetChildSessionStats(_ context.Context, _ int64) (int, int, error) {
	return 0, 0, nil
}

func (m *mockSessionStore) GetSessionTreeUsage(_ context.Context, _ int64) (int, int, float64, error) {
	return 0, 0, 0, nil
}

func (m *mockSessionStore) SaveContextBaseline(_ context.Context, _ int64, _ sessionstore.ContextBaseline) error {
	return nil
}

func (m *mockSessionStore) ClearContextBaseline(_ context.Context, _ int64) error {
	return nil
}

func (m *mockSessionStore) InsertToolNotificationPair(
	_ context.Context,
	_ int64,
	asst, result *transcript.Message,
) (int64, int64, error) {
	m.lastPairAsst = asst
	m.lastPairResult = result

	if m.pairErr != nil {
		return 0, 0, m.pairErr
	}

	m.nextMsgID += 2

	return m.nextMsgID - 1, m.nextMsgID, nil
}

func (m *mockSessionStore) InsertToolNotificationPairOnce(
	ctx context.Context,
	sessionID int64,
	_, _ string,
	asst, result *transcript.Message,
) (int64, int64, bool, error) {
	asstID, resultID, err := m.InsertToolNotificationPair(ctx, sessionID, asst, result)
	return asstID, resultID, err == nil, err
}

func (m *mockSessionStore) ResetSessionContext(
	ctx context.Context,
	sessionID int64,
	opening []*transcript.Message,
) ([]int64, error) {
	ids := make([]int64, len(opening))
	for i, message := range opening {
		id, err := m.InsertMessage(ctx, sessionID, message)
		if err != nil {
			return nil, err
		}
		ids[i] = id
	}

	return ids, nil
}

func (m *mockSessionStore) ResetSessionContextOnce(
	ctx context.Context,
	sessionID int64,
	_, _ string,
	opening []*transcript.Message,
) ([]int64, bool, error) {
	ids, err := m.ResetSessionContext(ctx, sessionID, opening)
	return ids, err == nil, err
}

// Mock LLM clients for session tests.

type mockLLMClient struct{}

func (m *mockLLMClient) Chat(
	_ context.Context,
	_ string,
	_ []llmwire.Message,
	_ []llmwire.ToolSchema,
	_ ...llmwire.ChatOption,
) (*llmwire.Response, error) {
	return &llmwire.Response{Text: "done"}, nil
}
func (m *mockLLMClient) Model() string                  { return "mock-model" }
func (m *mockLLMClient) APIKey() string                 { return "mock-key" }
func (m *mockLLMClient) Close() error                   { return nil }
func (m *mockLLMClient) Provider() string               { return testMockModel }
func (m *mockLLMClient) ContextWindow() int             { return 0 }
func (m *mockLLMClient) SetReasoningLevel(level string) {}
func (m *mockLLMClient) GetReasoningLevel() string      { return testReasoningLvl }

func (m *mockLLMClient) SetSessionID(id string) {}

// mockLLMRunOnce returns the response once on the first call, which causes
// the loop to produce a final text response and exit.
type mockLLMRunOnce struct {
	response *llmwire.Response
	called   bool
}

func (m *mockLLMRunOnce) Chat(
	_ context.Context,
	_ string,
	_ []llmwire.Message,
	_ []llmwire.ToolSchema,
	_ ...llmwire.ChatOption,
) (*llmwire.Response, error) {
	m.called = true
	return m.response, nil
}
func (m *mockLLMRunOnce) Model() string                  { return testMockModel }
func (m *mockLLMRunOnce) APIKey() string                 { return "" }
func (m *mockLLMRunOnce) Close() error                   { return nil }
func (m *mockLLMRunOnce) Provider() string               { return testMockModel }
func (m *mockLLMRunOnce) ContextWindow() int             { return 0 }
func (m *mockLLMRunOnce) SetReasoningLevel(level string) {}
func (m *mockLLMRunOnce) GetReasoningLevel() string      { return testReasoningLvl }

func (m *mockLLMRunOnce) SetSessionID(id string) {}

// mockLLMSequence returns responses in order.
type mockLLMSequence struct {
	responses []*llmwire.Response
	callCount int
}

func (m *mockLLMSequence) Chat(
	_ context.Context,
	_ string,
	_ []llmwire.Message,
	_ []llmwire.ToolSchema,
	_ ...llmwire.ChatOption,
) (*llmwire.Response, error) {
	idx := m.callCount
	m.callCount++
	if idx < len(m.responses) {
		return m.responses[idx], nil
	}
	return m.responses[len(m.responses)-1], nil
}
func (m *mockLLMSequence) Model() string                  { return testMockModel }
func (m *mockLLMSequence) APIKey() string                 { return "" }
func (m *mockLLMSequence) Close() error                   { return nil }
func (m *mockLLMSequence) Provider() string               { return testMockModel }
func (m *mockLLMSequence) ContextWindow() int             { return 0 }
func (m *mockLLMSequence) SetReasoningLevel(level string) {}
func (m *mockLLMSequence) GetReasoningLevel() string      { return testReasoningLvl }

func (m *mockLLMSequence) SetSessionID(id string) {}
