package session

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/tool/builtin"
)

const (
	testPrompt       = "test prompt"
	testReasoningLvl = "medium"
	testMockModel    = "mock"
)

// compactionMockLLM is a mock implementation for compaction tests.
type compactionMockLLM struct {
	response      *llmwire.Response
	err           error
	callCount     int
	lastMessages  []llmwire.Message
	contextWindow int
	// chat, when set, drives a call sequence and sees the rendered prompt.
	chat        func(callIndex int, prompt string) (*llmwire.Response, error)
	prompts     []string
	lastOptions llmwire.ChatOptions
}

type compactionRecordingStore struct {
	mockSessionStore
	nextID           int64
	messages         []*sessionstore.StoredMessage
	positions        map[int64]int
	markCompactedErr error
	markCompacted    int
	insertFailAt     int // 1-based InsertMessage call index that returns insertErr
	insertCalls      int
	insertErr        error
	replaceErr       error
}

func (m *compactionMockLLM) Chat(
	_ context.Context,
	_ string,
	messages []llmwire.Message,
	_ []llmwire.ToolSchema,
	opts ...llmwire.ChatOption,
) (*llmwire.Response, error) {
	m.callCount++
	m.lastOptions = llmwire.ApplyChatOptions(opts)
	m.lastMessages = messages

	prompt := ""
	if len(messages) > 0 {
		prompt = messages[0].Content
	}

	m.prompts = append(m.prompts, prompt)

	if m.chat != nil {
		return m.chat(m.callCount, prompt)
	}

	if m.err != nil {
		return nil, m.err
	}
	return m.response, nil
}
func (m *compactionMockLLM) Model() string             { return testMockModel }
func (m *compactionMockLLM) APIKey() string            { return "" }
func (m *compactionMockLLM) Close() error              { return nil }
func (m *compactionMockLLM) Provider() string          { return testMockModel }
func (m *compactionMockLLM) ContextWindow() int        { return m.contextWindow }
func (m *compactionMockLLM) SetReasoningLevel(string)  {}
func (m *compactionMockLLM) GetReasoningLevel() string { return testReasoningLvl }

func (m *compactionMockLLM) SetSessionID(id string) {}

func (s *compactionRecordingStore) InsertMessage(
	_ context.Context,
	sessionID int64,
	message *sessionstore.StoredMessage,
) (int64, error) {
	s.insertCalls++
	if s.insertErr != nil && (s.insertFailAt == 0 || s.insertCalls == s.insertFailAt) {
		return 0, s.insertErr
	}

	stored := *message
	stored.ID = s.nextID
	stored.SessionID = sessionID
	s.nextID++
	s.messages = append(s.messages, &stored)
	if s.positions == nil {
		s.positions = make(map[int64]int)
	}

	s.positions[stored.ID] = int(stored.ID)

	return stored.ID, nil
}

func (s *compactionRecordingStore) MarkCompacted(_ context.Context, ids []int64) error {
	s.markCompacted++
	if s.markCompactedErr != nil {
		return s.markCompactedErr
	}

	now := time.Now()
	compacted := make(map[int64]bool, len(ids))
	for _, id := range ids {
		compacted[id] = true
	}

	for _, message := range s.messages {
		if compacted[message.ID] {
			message.CompactedAt = &now
		}
	}

	return nil
}

func (s *compactionRecordingStore) ReplaceCompactedMessages(
	ctx context.Context,
	sessionID int64,
	compactedIDs []int64,
	entries []sessionstore.CompactionEntry,
) ([]int64, error) {
	if s.replaceErr != nil {
		return nil, s.replaceErr
	}

	if err := s.MarkCompacted(ctx, compactedIDs); err != nil {
		return nil, err
	}

	ids := make([]int64, len(entries))
	for i, entry := range entries {
		id := entry.ExistingID
		if id == 0 {
			var err error

			id, err = s.InsertMessage(ctx, sessionID, entry.Message)
			if err != nil {
				return nil, err
			}
		}

		ids[i] = id
		s.positions[id] = i + 1
	}

	return ids, nil
}

func (s *compactionRecordingStore) LoadActiveMessages(
	_ context.Context,
	sessionID int64,
) ([]*sessionstore.StoredMessage, error) {
	var active []*sessionstore.StoredMessage
	for _, message := range s.messages {
		if message.SessionID != sessionID || message.CompactedAt != nil {
			continue
		}

		stored := *message
		active = append(active, &stored)
	}

	slices.SortFunc(active, func(a, b *sessionstore.StoredMessage) int {
		return cmp.Compare(s.positions[a.ID], s.positions[b.ID])
	})

	return active, nil
}

func (s *compactionRecordingStore) ResetSessionContextOnce(
	ctx context.Context,
	sessionID int64,
	_, _ string,
	opening []*sessionstore.StoredMessage,
) ([]int64, bool, error) {
	ids, err := s.resetSessionContext(ctx, sessionID, opening)
	return ids, err == nil, err
}

func (s *compactionRecordingStore) resetSessionContext(
	ctx context.Context,
	sessionID int64,
	opening []*sessionstore.StoredMessage,
) ([]int64, error) {
	// Model the production transaction: failure restores every mutation.
	beforeMessages := make([]*sessionstore.StoredMessage, len(s.messages))
	for i, message := range s.messages {
		copyMessage := *message
		beforeMessages[i] = &copyMessage
	}
	beforeNextID := s.nextID
	beforePositions := maps.Clone(s.positions)
	beforeInsertCalls := s.insertCalls
	beforeMarkCompacted := s.markCompacted
	rollback := func() {
		s.messages = beforeMessages
		s.nextID = beforeNextID
		s.positions = beforePositions
		s.insertCalls = beforeInsertCalls
		s.markCompacted = beforeMarkCompacted
	}

	oldIDs := make([]int64, 0, len(s.messages))
	for _, message := range s.messages {
		if message.SessionID == sessionID && message.CompactedAt == nil {
			oldIDs = append(oldIDs, message.ID)
		}
	}
	if err := s.MarkCompacted(ctx, oldIDs); err != nil {
		rollback()
		return nil, err
	}

	ids := make([]int64, len(opening))
	for i, message := range opening {
		id, err := s.InsertMessage(ctx, sessionID, message)
		if err != nil {
			rollback()
			return nil, err
		}
		ids[i] = id
	}

	return ids, nil
}

// compactIfNeeded drives the automatic path the way applyContextEvents does,
// for tests that want the trigger without a loop runner around it.
func (s *svc) compactIfNeeded(ctx context.Context, window int) error {
	if !s.shouldCompact(window) {
		return nil
	}

	_, err := s.compact(ctx, nil)

	return err
}

func newCompactionTestSvc(mockLLM *compactionMockLLM) *svc {
	return &svc{
		llmClient:    mockLLM,
		ms:           newMessageStore(nil, 0),
		loopDetector: newLoopDetector(),
		registry:     tool.NewRegistry(),
		prompt:       newPromptBuilder(testPrompt, "", ""),
	}
}

func TestCompactIfNeeded_BelowThreshold_NoCompaction(t *testing.T) {
	mockLLM := &compactionMockLLM{response: &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop}}
	s := newCompactionTestSvc(mockLLM)

	require.NoError(t, s.ms.addUserMessage(context.Background(), "Hello"))
	require.NoError(t, s.ms.addAssistantMessage(context.Background(), &llmwire.Response{Text: "Hi"}))
	require.NoError(t, s.ms.addUserMessage(context.Background(), "How are you?"))

	err := s.compactIfNeeded(context.Background(), 100000)
	require.NoError(t, err)
	assert.Equal(t, 0, mockLLM.callCount)
	assert.Len(t, s.ms.getMessages(), 3)
}

func TestCompactIfNeeded_AboveThreshold_Compacts(t *testing.T) {
	mockLLM := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: 32000,
	}
	s := newCompactionTestSvc(mockLLM)
	s.ms.setMessages(oversizedTranscript(32000))

	err := s.compactIfNeeded(context.Background(), 32000)
	require.NoError(t, err)
	assert.Equal(t, 1, mockLLM.callCount)

	// Header -> marked summary -> verbatim tail (at least one round survives).
	messages := s.ms.getMessages()
	require.Greater(t, len(messages), 3, "a verbatim tail survives the checkpoint")

	assert.Equal(t, llmwire.RoleSystem, messages[0].Role)
	assert.Equal(t, llmwire.RoleUser, messages[1].Role)
	assert.Equal(t, llmwire.RoleUser, messages[2].Role)
	assert.True(t, isMarkedSummary(messages[2].Content), "the marked summary follows the header")
	for _, m := range messages[3:] {
		assert.NotEqual(t, llmwire.RoleUser, m.Role, "only the summary row sits between header and tail")
	}
}

// A summarization that never produced an accepted summary must leave the
// conversation exactly as it was.
func TestCompactIfNeeded_SummaryFailure_KeepsTheConversation(t *testing.T) {
	mockLLM := &compactionMockLLM{err: errors.New("summary generation failed")}
	s := newCompactionTestSvc(mockLLM)

	s.ms.setMessages(oversizedTranscript(32000))

	before := s.ms.getMessages()

	err := s.compactIfNeeded(context.Background(), 32000)
	require.Error(t, err)

	after := s.ms.getMessages()
	require.Len(t, after, len(before), "no summarizing rewrite without a summary")
	for i := range before {
		assert.Equal(t, before[i].Role, after[i].Role)
		assert.Equal(t, before[i].Content, after[i].Content)
	}
}

func TestCompactionAttributesOwnCostToSummaryRow(t *testing.T) {
	ctx := context.Background()
	store := &compactionRecordingStore{nextID: 1}
	mockLLM := &compactionMockLLM{
		chat: func(_ int, _ string) (*llmwire.Response, error) {
			return &llmwire.Response{
				Text:       validSummary,
				FinishType: llmwire.FinishStop,
				CostUSD:    0.01,
				Usage:      &llmwire.MessageUsage{PromptTokens: 100, CompletionTokens: 20},
			}, nil
		},
	}
	s := newCompactionTestSvc(mockLLM)
	s.ms = newMessageStore(store, 1)

	// Costed rounds big enough to cross the trigger.
	msgs := []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		compactionUserMessage("task"),
	}
	for estimateTokens(msgs) < compactionCutoff(32000)+10000 {
		id := fmt.Sprintf("cost-%d", len(msgs))
		msgs = append(msgs, llmwire.Message{
			Role: llmwire.RoleAssistant, Content: "step", CostUSD: 0.5,
			ToolCalls: []llmwire.ToolCall{{ID: id, Name: "read"}},
		})
		msgs = append(msgs, llmwire.Message{
			Role: llmwire.RoleTool, Content: strings.Repeat("t", 3600), ToolCallID: id, ToolName: "read",
		})
	}

	for i := range msgs {
		message := msgs[i]
		s.ms.mu.Lock()
		require.NoError(t, s.ms.appendMessageLocked(ctx, &message))
		s.ms.mu.Unlock()
	}

	require.NoError(t, s.compactIfNeeded(ctx, 32000))
	require.Equal(t, 1, mockLLM.callCount, "one summarization call")

	summary := findStoredSummary(t, store)
	assert.InDelta(t, 0.01, summary.CostUSD, 1e-9,
		"summary row carries the summed compaction cost")

	var usage llmwire.MessageUsage
	require.NoError(t, json.Unmarshal(summary.Usage, &usage))
	assert.Equal(t, 100, usage.PromptTokens)
	assert.Equal(t, 20, usage.CompletionTokens)

	var originalCost float64

	for _, m := range store.messages {
		if m.Role == llmwire.RoleAssistant && m.CompactedAt != nil {
			originalCost += m.CostUSD
		}
	}

	assert.Positive(t, originalCost, "compacted originals keep their own cost")
	var compactedAssistants int

	for _, m := range store.messages {
		if m.Role == llmwire.RoleAssistant && m.CompactedAt != nil {
			compactedAssistants++
		}
	}

	assert.InDelta(t, 0.5*float64(compactedAssistants), originalCost, 1e-9,
		"each compacted original keeps its own cost, counted exactly once")
}

// findStoredSummary returns the persisted marked summary row.
func findStoredSummary(t *testing.T, store *compactionRecordingStore) *sessionstore.StoredMessage {
	t.Helper()

	for _, m := range store.messages {
		if isMarkedSummary(m.Content) {
			return m
		}
	}

	t.Fatal("summary row not found in store")

	return nil
}

func renderedSkills(messages []llmwire.Message) []llmwire.Message {
	var skills []llmwire.Message
	for _, message := range messages {
		for _, invocation := range builtin.ExtractRenderedSkills(message.Content) {
			skillMessage := message
			skillMessage.Content = invocation.Envelope
			skills = append(skills, skillMessage)
		}
	}

	return skills
}

func skillMessage(t *testing.T, name, content string) llmwire.Message {
	t.Helper()

	return llmwire.Message{
		Role:    llmwire.RoleUser,
		Content: builtin.RenderSkill(&loader.Skill{Name: name, Content: content}, ""),
	}
}
