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

	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool/builtin"
	"github.com/pilat/coagent/internal/transcript"
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
	messages         []*transcript.Message
	positions        map[int64]int
	markCompactedErr error
	markCompacted    int
	insertFailAt     int // 1-based appendRow call index that returns insertErr
	insertCalls      int
	insertErr        error
	replaceErr       error
}

func (s *compactionRecordingStore) Commit(
	ctx context.Context,
	c sessionstore.Commit,
) (*sessionstore.CommitResult, error) {
	beforeMessages := make([]*transcript.Message, len(s.messages))
	for i, row := range s.messages {
		copyRow := *row
		beforeMessages[i] = &copyRow
	}
	beforePositions := maps.Clone(s.positions)
	beforeID := s.nextID
	result := &sessionstore.CommitResult{}
	if c.Replace != nil {
		if _, err := s.replaceRows(ctx, c.SessionID, c.Replace.HeadIDs, c.Replace.Entries); err != nil {
			s.messages = beforeMessages
			s.positions = beforePositions
			s.nextID = beforeID
			return nil, err
		}
	}
	for _, row := range append(append(append([]*transcript.Message{}, c.Messages...), c.Unfired.Messages...), c.ToolResults...) {
		id, err := s.appendRow(ctx, c.SessionID, row)
		if err != nil {
			s.messages = beforeMessages
			s.positions = beforePositions
			s.nextID = beforeID
			return nil, err
		}
		result.MessageIDs = append(result.MessageIDs, id)
	}
	for _, output := range c.Outputs {
		result.Outputs = append(result.Outputs, &sessionstore.OutputCommit{Content: output.Content})
	}
	return result, nil
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
func (m *compactionMockLLM) Model() string                          { return testMockModel }
func (m *compactionMockLLM) APIKey() string                         { return "" }
func (m *compactionMockLLM) Close() error                           { return nil }
func (m *compactionMockLLM) Provider() string                       { return testMockModel }
func (m *compactionMockLLM) ContextWindow() int                     { return m.contextWindow }
func (m *compactionMockLLM) SetReasoningLevel(string)               {}
func (m *compactionMockLLM) SetImageAuthorizer(llm.ImageAuthorizer) {}
func (m *compactionMockLLM) GetReasoningLevel() string              { return testReasoningLvl }

func (m *compactionMockLLM) SetSessionID(id string) {}

func (s *compactionRecordingStore) appendRow(
	_ context.Context,
	sessionID int64,
	message *transcript.Message,
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

func (s *compactionRecordingStore) markRowsCompacted(_ context.Context, ids []int64) error {
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

func (s *compactionRecordingStore) replaceRows(
	ctx context.Context,
	sessionID int64,
	compactedIDs []int64,
	entries []sessionstore.CompactionEntry,
) ([]int64, error) {
	if s.replaceErr != nil {
		return nil, s.replaceErr
	}

	if err := s.markRowsCompacted(ctx, compactedIDs); err != nil {
		return nil, err
	}

	ids := make([]int64, len(entries))
	for i, entry := range entries {
		id := entry.ExistingID
		if id == 0 {
			var err error

			id, err = s.appendRow(ctx, sessionID, entry.Message)
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
) ([]*transcript.Message, error) {
	var active []*transcript.Message
	for _, message := range s.messages {
		if message.SessionID != sessionID || message.CompactedAt != nil {
			continue
		}

		stored := *message
		active = append(active, &stored)
	}

	slices.SortFunc(active, func(a, b *transcript.Message) int {
		return cmp.Compare(s.positions[a.ID], s.positions[b.ID])
	})

	return active, nil
}

func (s *Session) compactIfNeeded(ctx context.Context, window int) error {
	if !s.shouldCompact(window) {
		return nil
	}

	_, err := s.compact(ctx, nil)

	return err
}

func newCompactionTestSvc(mockLLM *compactionMockLLM) *Session {
	s := newTestAgent()
	s.llmClient = mockLLM
	return s
}

func TestCompactIfNeeded_BelowThreshold_NoCompaction(t *testing.T) {
	mockLLM := &compactionMockLLM{response: &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop}}
	s := newCompactionTestSvc(mockLLM)

	require.NoError(t, appendTestUser(context.Background(), s.ms, "Hello"))
	require.NoError(t, appendTestAssistant(context.Background(), s.ms, &llmwire.Response{Text: "Hi"}))
	require.NoError(t, appendTestUser(context.Background(), s.ms, "How are you?"))

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
	setTestMessages(s, oversizedTranscript(32000))

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

	setTestMessages(s, oversizedTranscript(32000))

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
	s.store = store
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
		require.NoError(t, appendTestMessage(ctx, s.ms, &message))
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
func findStoredSummary(t *testing.T, store *compactionRecordingStore) *transcript.Message {
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
		for _, invocation := range loader.ExtractRenderedSkills(message.Content) {
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
