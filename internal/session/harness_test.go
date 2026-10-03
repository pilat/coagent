package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionprompt"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/transcript"
)

var errStoreDown = errors.New("store unavailable")

type mockSessionStore struct {
	sessionstore.Store
	nextMsgID                 int64
	messages                  []*transcript.Message
	insertCalls, insertFailAt int
	insertErr                 error
	completion                sessionstore.CompletionCheckState
}

func (m *mockSessionStore) Commit(_ context.Context, c sessionstore.Commit) (*sessionstore.CommitResult, error) {
	if c.Replace != nil {
		byID := map[int64]*transcript.Message{}
		for _, message := range m.messages {
			byID[message.ID] = message
		}
		m.messages = nil
		for _, entry := range c.Replace.Entries {
			if entry.ExistingID != 0 {
				m.messages = append(m.messages, byID[entry.ExistingID])
				continue
			}
			m.nextMsgID++
			row := *entry.Message
			row.ID = m.nextMsgID
			row.SessionID = c.SessionID
			m.messages = append(m.messages, &row)
		}
	}
	batch := append(append(append([]*transcript.Message{}, c.Messages...), c.Unfired.Messages...), c.ToolResults...)
	for range batch {
		m.insertCalls++
		if m.insertErr != nil && (m.insertFailAt == 0 || m.insertCalls == m.insertFailAt) {
			return nil, m.insertErr
		}
	}
	result := &sessionstore.CommitResult{}
	for _, message := range batch {
		m.nextMsgID++
		copyMessage := *message
		copyMessage.ID = m.nextMsgID
		copyMessage.SessionID = c.SessionID
		m.messages = append(m.messages, &copyMessage)
		result.MessageIDs = append(result.MessageIDs, copyMessage.ID)
	}
	for _, output := range append(c.Outputs, c.Unfired.Outputs...) {
		result.Outputs = append(result.Outputs, &sessionstore.OutputCommit{Content: output.Content})
	}
	for _, state := range []sessionstore.StatePatch{c.Unfired.State, c.State} {
		m.applyCandidate(state.Candidate, result.MessageIDs)

		if state.EmptyStopStreak != nil {
			m.completion.EmptyStopStreak = *state.EmptyStopStreak
		}
	}
	return result, nil
}

func (m *mockSessionStore) LoadActiveMessages(context.Context, int64) ([]*transcript.Message, error) {
	return m.messages, nil
}

func (*mockSessionStore) ListPending(context.Context, int64) ([]*sessionstore.InboxInput, error) {
	return nil, nil
}

func (*mockSessionStore) PendingActivation(context.Context, int64) (*sessionstore.ToolActivation, error) {
	return nil, sessionstore.ErrActivationNotFound
}

func (*mockSessionStore) HasBackgroundWakeSource(context.Context, int64) (bool, error) {
	return false, nil
}

func (m *mockSessionStore) LoadCompletionCheckState(
	context.Context,
	int64,
) (*sessionstore.CompletionCheckState, error) {
	return &m.completion, nil
}

func (*mockSessionStore) HasOutstandingResponseRecovery(context.Context, int64) (bool, error) {
	return false, nil
}

func (*mockSessionStore) GetChildSessionStats(context.Context, int64) (int, int, error) {
	return 0, 0, nil
}

func (*mockSessionStore) GetSessionTreeUsage(context.Context, int64) (int, int, float64, error) {
	return 0, 0, 0, nil
}

func newTestAgent(tools ...tool.Tool) *Session {
	reg := tool.NewRegistry()
	for _, tt := range tools {
		reg.Register(tt)
	}
	store := &mockSessionStore{}
	prompt := sessionprompt.NewBuilder("test", "")
	prompt.Todos = todo.New()
	return &Session{
		id:           1,
		rootID:       1,
		llmClient:    &mockLLMRunOnce{response: textResponse("ok")},
		loader:       loader.New(),
		store:        store,
		ms:           newMessageStore(store, 1),
		loopDetector: newLoopDetector(),
		registry:     reg,
		prompt:       prompt,
	}
}

func setTestMessages(s *Session, messages []llmwire.Message) {
	store := s.ms.store
	switch m := store.(type) {
	case *mockSessionStore:
		m.messages = nil
		m.nextMsgID = 0
	case *compactionRecordingStore:
		m.messages = nil
		m.nextID = 1
		m.positions = nil
	}
	rows := make([]*transcript.Message, 0, len(messages))
	for i := range messages {
		row, err := storedMessage(&messages[i])
		if err != nil {
			panic(err)
		}
		rows = append(rows, row)
	}
	if _, err := store.Commit(context.Background(), sessionstore.Commit{SessionID: s.id, Messages: rows}); err != nil {
		panic(err)
	}
	s.store = store
	if err := s.ms.reloadMessages(context.Background()); err != nil {
		panic(err)
	}
}

func appendTestMessage(ctx context.Context, ms *messageStore, message *llmwire.Message) error {
	row, err := storedMessage(message)
	if err != nil {
		return err
	}
	if _, err := ms.store.Commit(
		ctx,
		sessionstore.Commit{SessionID: ms.sessID, Messages: []*transcript.Message{row}},
	); err != nil {
		return err
	}
	return ms.reloadMessages(ctx)
}

func appendTestUser(ctx context.Context, ms *messageStore, text string) error {
	return appendTestMessage(ctx, ms, &llmwire.Message{Role: llmwire.RoleUser, Content: text})
}

func appendTestAssistant(ctx context.Context, ms *messageStore, r *llmwire.Response) error {
	return appendTestMessage(
		ctx,
		ms,
		&llmwire.Message{
			Role:         llmwire.RoleAssistant,
			Content:      r.Text,
			ToolCalls:    r.ToolCalls,
			ReasoningRaw: r.ReasoningRaw,
		},
	)
}

func runTestLoop(t *testing.T, s *Session) (RunResult, error) {
	t.Helper()
	if len(s.ms.getMessages()) == 0 {
		setTestMessages(s, []llmwire.Message{usr("task")})
	}
	return s.Run(t.Context())
}

type noteEvents struct{ notes *[]string }

func (e noteEvents) Emit(n sessionevent.Notification) {
	if n.Type == sessionevent.NotifyMessage {
		*e.notes = append(*e.notes, n.Message)
	}
}

type mockLLMRunOnce struct {
	response *llmwire.Response
	called   bool
}

func (m *mockLLMRunOnce) Chat(
	context.Context,
	string,
	[]llmwire.Message,
	[]llmwire.ToolSchema,
	...llmwire.ChatOption,
) (*llmwire.Response, error) {
	m.called = true
	return normalizeScriptedResponse(m.response), nil
}
func (*mockLLMRunOnce) Model() string                          { return testMockModel }
func (*mockLLMRunOnce) APIKey() string                         { return "" }
func (*mockLLMRunOnce) Close() error                           { return nil }
func (*mockLLMRunOnce) Provider() string                       { return testMockModel }
func (*mockLLMRunOnce) ContextWindow() int                     { return 0 }
func (*mockLLMRunOnce) SetReasoningLevel(string)               {}
func (*mockLLMRunOnce) GetReasoningLevel() string              { return testReasoningLvl }
func (*mockLLMRunOnce) SetSessionID(string)                    {}
func (*mockLLMRunOnce) SetImageAuthorizer(llm.ImageAuthorizer) {}

func textResponse(text string) *llmwire.Response {
	return &llmwire.Response{Text: text, FinishType: llmwire.FinishStop}
}

func toolCallResponse(id, name string) *llmwire.Response {
	return &llmwire.Response{
		ToolCalls:  []llmwire.ToolCall{{ID: id, Name: name, Arguments: json.RawMessage(`{}`)}},
		FinishType: llmwire.FinishToolCalls,
	}
}

func normalizeScriptedResponse(r *llmwire.Response) *llmwire.Response {
	if r != nil && r.FinishType == "" {
		copyResponse := *r
		if len(r.ToolCalls) > 0 {
			copyResponse.FinishType = llmwire.FinishToolCalls
		} else {
			copyResponse.FinishType = llmwire.FinishStop
		}
		return &copyResponse
	}
	return r
}

type loopScriptLLM struct {
	mockLLMRunOnce
	responses    []*llmwire.Response
	calls        int
	err          error
	lastTools    []llmwire.ToolSchema
	lastMessages []llmwire.Message
	onCall       func(int, []llmwire.Message) (*llmwire.Response, error)
}

func (m *loopScriptLLM) Chat(
	_ context.Context,
	_ string,
	messages []llmwire.Message,
	tools []llmwire.ToolSchema,
	_ ...llmwire.ChatOption,
) (*llmwire.Response, error) {
	m.calls++
	m.lastTools = tools
	m.lastMessages = messages
	if m.onCall != nil {
		r, err := m.onCall(m.calls, messages)
		return normalizeScriptedResponse(r), err
	}
	if m.err != nil {
		return nil, m.err
	}
	if m.calls > len(m.responses) {
		return nil, fmt.Errorf("unexpected model call %d", m.calls)
	}
	return normalizeScriptedResponse(m.responses[m.calls-1]), nil
}

func usr(content string) llmwire.Message {
	return llmwire.Message{Role: llmwire.RoleUser, Content: content}
}

func asst(content string, calls ...llmwire.ToolCall) llmwire.Message {
	return llmwire.Message{Role: llmwire.RoleAssistant, Content: content, ToolCalls: calls}
}

func call(id, name string) llmwire.ToolCall {
	return llmwire.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(`{}`)}
}

func toolRes(id string) llmwire.Message {
	return llmwire.Message{Role: llmwire.RoleTool, ToolCallID: id, Content: "done"}
}

func transcriptText(messages []llmwire.Message) string {
	var out string
	for _, m := range messages {
		out += m.Content
	}
	return out
}

func hasSummaryRow(messages []llmwire.Message) bool {
	for _, m := range messages {
		if isMarkedSummary(m.Content) {
			return true
		}
	}
	return false
}

func (m *mockSessionStore) applyCandidate(change *sessionstore.CandidateChange, ids []int64) {
	if change == nil {
		return
	}
	next := change.Next
	if change.NextRef >= 0 {
		next = ids[change.NextRef]
	}
	m.completion.CandidateText = ""
	if next == 0 {
		m.completion.CandidateID = nil
		return
	}
	m.completion.CandidateID = &next
	for _, row := range m.messages {
		if row.ID == next {
			m.completion.CandidateText = row.Content
		}
	}
}
