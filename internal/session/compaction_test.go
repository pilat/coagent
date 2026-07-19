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
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/registry"
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
	markClearedErr   error
	markCompactedErr error
	markCompacted    int
	insertFailAt     int // 1-based InsertMessage call index that returns insertErr
	insertCalls      int
	insertErr        error
	updateBriefErr   error
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

func (s *compactionRecordingStore) UpdateSessionCompactionBrief(_ context.Context, _ int64, _ string) error {
	return s.updateBriefErr
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
func (s *svc) compactIfNeeded(ctx context.Context, window, keepRecent int) error {
	if !s.shouldCompact(window) {
		return nil
	}

	_, err := s.compact(ctx, keepRecent)

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
	mockLLM := &compactionMockLLM{response: &llmwire.Response{Text: "test summary"}}
	s := newCompactionTestSvc(mockLLM)

	require.NoError(t, s.ms.addUserMessage(context.Background(), "Hello"))
	require.NoError(t, s.ms.addAssistantMessage(context.Background(), &llmwire.Response{Text: "Hi"}))
	require.NoError(t, s.ms.addUserMessage(context.Background(), "How are you?"))

	err := s.compactIfNeeded(context.Background(), 100000, 6)
	require.NoError(t, err)
	assert.Equal(t, 0, mockLLM.callCount)
	assert.Len(t, s.ms.getMessages(), 3)
}

func TestCompactIfNeeded_AboveThreshold_Compacts(t *testing.T) {
	mockLLM := &compactionMockLLM{
		response: &llmwire.Response{
			Text: "## Goal\nBuild feature\n## Progress\n- Explored codebase\n## Context for Continuation\nContinue",
		},
	}
	s := newCompactionTestSvc(mockLLM)

	s.ms.setMessages([]llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "You are a helpful assistant"},
		{Role: llmwire.RoleUser, Content: "Initial task: build a feature"},
		{Role: llmwire.RoleAssistant, Content: "Exploring", ToolCalls: []llmwire.ToolCall{{ID: "c1", Name: "glob"}}},
		{Role: llmwire.RoleTool, Content: "found 10 files", ToolCallID: "c1", ToolName: "glob"},
		{Role: llmwire.RoleAssistant, Content: "Reading", ToolCalls: []llmwire.ToolCall{{ID: "c2", Name: "read"}}},
		{Role: llmwire.RoleTool, Content: "package main...", ToolCallID: "c2", ToolName: "read"},
		{
			Role:      llmwire.RoleAssistant,
			Content:   "Checking config",
			ToolCalls: []llmwire.ToolCall{{ID: "c3", Name: "read"}},
		},
		{Role: llmwire.RoleTool, Content: "package config...", ToolCallID: "c3", ToolName: "read"},
		{
			Role:      llmwire.RoleAssistant,
			Content:   "Implementing",
			ToolCalls: []llmwire.ToolCall{{ID: "c4", Name: "write"}},
		},
		{Role: llmwire.RoleTool, Content: "written successfully", ToolCallID: "c4", ToolName: "write"},
	})

	err := s.compactIfNeeded(context.Background(), 1, 2) // summarizer sees 2 rounds unclear
	require.NoError(t, err)
	assert.Equal(t, 1, mockLLM.callCount)

	// Header + summary + ack + primer, and nothing verbatim behind them.
	messages := s.ms.getMessages()
	assert.Len(t, messages, 5)

	assert.Equal(t, llmwire.RoleSystem, messages[0].Role)
	assert.Equal(t, llmwire.RoleUser, messages[1].Role)
	assert.Equal(t, llmwire.RoleUser, messages[2].Role)
	assert.Contains(t, messages[2].Content, "CONTEXT SUMMARY")
	assert.Equal(t, llmwire.RoleAssistant, messages[3].Role)
	assert.Equal(t, registry.PostCompactionAssistantAck, messages[3].Content)
}

// A summarization that never produced a summary must leave the conversation
// exactly as it was — the transcript is the work, and a note saying the work was
// lost is not a summary of it.
func TestCompactIfNeeded_SummaryFailure_KeepsTheConversation(t *testing.T) {
	mockLLM := &compactionMockLLM{err: errors.New("summary generation failed")}
	s := newCompactionTestSvc(mockLLM)

	s.ms.setMessages([]llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "You are a helpful assistant"},
		{Role: llmwire.RoleUser, Content: "Initial task: build a feature"},
		{Role: llmwire.RoleAssistant, Content: "I'll help", ToolCalls: []llmwire.ToolCall{{ID: "c1", Name: "glob"}}},
		{Role: llmwire.RoleTool, Content: "found files", ToolCallID: "c1", ToolName: "glob"},
		{
			Role:      llmwire.RoleAssistant,
			Content:   "Now let me read",
			ToolCalls: []llmwire.ToolCall{{ID: "c2", Name: "read"}},
		},
		{Role: llmwire.RoleTool, Content: "content", ToolCallID: "c2", ToolName: "read"},
		{
			Role:      llmwire.RoleAssistant,
			Content:   "Let me write",
			ToolCalls: []llmwire.ToolCall{{ID: "c3", Name: "write"}},
		},
		{Role: llmwire.RoleTool, Content: "done", ToolCallID: "c3", ToolName: "write"},
	})

	before := s.ms.getMessages()

	err := s.compactIfNeeded(context.Background(), 1, 1)
	require.Error(t, err)

	after := s.ms.getMessages()
	require.Len(t, after, len(before), "no summarizing rewrite without a summary")
	for i := range before {
		assert.Equal(t, before[i].Role, after[i].Role)
	}

	assert.Empty(t, s.compactionBrief)
}

func TestCompactIfNeeded_IncrementalMerge(t *testing.T) {
	mockLLM := &compactionMockLLM{
		response: &llmwire.Response{
			Text: "## Goal\nBuild X\n## Progress\n- Step 1 done\n## Context for Continuation\nContinue",
		},
	}
	s := newCompactionTestSvc(mockLLM)

	s.ms.setMessages([]llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		{Role: llmwire.RoleUser, Content: "task"},
		{Role: llmwire.RoleAssistant, Content: "step1", ToolCalls: []llmwire.ToolCall{{ID: "c1", Name: "read"}}},
		{Role: llmwire.RoleTool, Content: "result1", ToolCallID: "c1", ToolName: "read"},
		{Role: llmwire.RoleAssistant, Content: "step2", ToolCalls: []llmwire.ToolCall{{ID: "c2", Name: "write"}}},
		{Role: llmwire.RoleTool, Content: "result2", ToolCallID: "c2", ToolName: "write"},
		{Role: llmwire.RoleAssistant, Content: "step3", ToolCalls: []llmwire.ToolCall{{ID: "c3", Name: "bash"}}},
		{Role: llmwire.RoleTool, Content: "result3", ToolCallID: "c3", ToolName: "bash"},
	})

	// First compaction
	err := s.compactIfNeeded(context.Background(), 1, 1)
	require.NoError(t, err)
	assert.Equal(t, 1, mockLLM.callCount)
	assert.Contains(t, s.compactionBrief, "## Goal")

	s.ms.mu.Lock()
	s.ms.messages = append(
		s.ms.messages,
		llmwire.Message{
			Role:      llmwire.RoleAssistant,
			Content:   "step4",
			ToolCalls: []llmwire.ToolCall{{ID: "c4", Name: "read"}},
		},
		llmwire.Message{Role: llmwire.RoleTool, Content: "result4", ToolCallID: "c4", ToolName: "read"},
		llmwire.Message{
			Role:      llmwire.RoleAssistant,
			Content:   "step5",
			ToolCalls: []llmwire.ToolCall{{ID: "c5", Name: "write"}},
		},
		llmwire.Message{Role: llmwire.RoleTool, Content: "result5", ToolCallID: "c5", ToolName: "write"},
	)
	s.ms.mu.Unlock()

	// Second compaction — merge
	mockLLM.response = &llmwire.Response{
		Text: "## Goal\nBuild X\n## Progress\n- Step 1 done\n- Step 4-5 done\n## Context for Continuation\nContinue",
	}
	mockLLM.callCount = 0
	err = s.compactIfNeeded(context.Background(), 1, 1)
	require.NoError(t, err)
	assert.Equal(t, 1, mockLLM.callCount)

	require.NotEmpty(t, mockLLM.lastMessages)
	assert.Contains(t, mockLLM.lastMessages[0].Content, "EXISTING BRIEF:")
}

// TestCompactionAttributesOwnCostToSummaryRow verifies the summary row carries the
// compaction's own LLM cost/usage, while the compacted originals keep their own
// cost_usd — so a lifetime tree-sum counts each exactly once instead of
// double-counting a rolled-up total.
func TestCompactionAttributesOwnCostToSummaryRow(t *testing.T) {
	ctx := context.Background()
	store := &compactionRecordingStore{nextID: 1}
	mockLLM := &compactionMockLLM{
		chat: func(_ int, _ string) (*llmwire.Response, error) {
			return &llmwire.Response{
				Text:    validSummary,
				CostUSD: 0.01,
				Usage:   &llmwire.MessageUsage{PromptTokens: 100, CompletionTokens: 20},
			}, nil
		},
	}
	s := newCompactionTestSvc(mockLLM)
	s.ms = newMessageStore(store, 1)

	big := strings.Repeat("x", 20000)

	msgs := []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		{Role: llmwire.RoleUser, Content: "task"},
	}

	for i := range 3 {
		id := fmt.Sprintf("c%d", i)
		msgs = append(msgs,
			llmwire.Message{
				Role: llmwire.RoleAssistant, Content: "step", CostUSD: 0.5,
				ToolCalls: []llmwire.ToolCall{{ID: id, Name: "read"}},
			},
			llmwire.Message{Role: llmwire.RoleTool, Content: big, ToolCallID: id, ToolName: "read"},
		)
	}

	for i := range msgs {
		m := msgs[i]
		s.ms.mu.Lock()
		require.NoError(t, s.ms.appendMessageLocked(ctx, &m))
		s.ms.mu.Unlock()
	}

	require.NoError(t, s.compactIfNeeded(ctx, 1, 1))
	require.Equal(t, 1, mockLLM.callCount, "one summarization call")

	summary := findStoredSummary(t, store)
	assert.InDelta(t, 0.01*float64(mockLLM.callCount), summary.CostUSD, 1e-9,
		"summary row carries the summed compaction cost")

	var usage llmwire.MessageUsage
	require.NoError(t, json.Unmarshal(summary.Usage, &usage))
	assert.Equal(t, 100*mockLLM.callCount, usage.PromptTokens)
	assert.Equal(t, 20*mockLLM.callCount, usage.CompletionTokens)

	var originalCost float64

	for _, m := range store.messages {
		if m.Role == llmwire.RoleAssistant && m.CompactedAt != nil {
			originalCost += m.CostUSD
		}
	}

	assert.InDelta(t, 1.5, originalCost, 1e-9, "all 3 compacted assistant originals × 0.5, kept intact")
}

// findStoredSummary returns the persisted [CONTEXT SUMMARY row.
func findStoredSummary(t *testing.T, store *compactionRecordingStore) *sessionstore.StoredMessage {
	t.Helper()

	for _, m := range store.messages {
		if strings.HasPrefix(m.Content, "[CONTEXT SUMMARY") {
			return m
		}
	}

	t.Fatal("summary row not found in store")

	return nil
}

func TestCompactionWithConfigConstants(t *testing.T) {
	assert.Positive(t, compactionThreshold)
	assert.Positive(t, compactionKeepRecent)
	assert.Less(t, compactionKeepRecent, 100)
}

func TestCompactionReattachesDirectSkillAsUserContext(t *testing.T) {
	mockLLM := &compactionMockLLM{response: &llmwire.Response{
		Text: "## Goal\nReview\n## Progress\n- Started\n## Context for Continuation\nContinue",
	}}
	s := newCompactionTestSvc(mockLLM)
	rendered := builtin.RenderSkill(&loader.Skill{Name: "review", Content: "Review carefully."}, "")

	s.ms.setMessages([]llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		{Role: llmwire.RoleUser, Content: "task"},
		{Role: llmwire.RoleUser, Content: "[timestamp] " + rendered},
		{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "c1", Name: "read"}}},
		{Role: llmwire.RoleTool, Content: "one", ToolCallID: "c1", ToolName: "read"},
		{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "c2", Name: "read"}}},
		{Role: llmwire.RoleTool, Content: "two", ToolCallID: "c2", ToolName: "read"},
	})

	err := s.compactIfNeeded(context.Background(), 1, 1)
	require.NoError(t, err)

	messages := s.ms.getMessages()
	require.GreaterOrEqual(t, len(messages), 6)
	assert.Equal(t, llmwire.RoleUser, messages[5].Role)
	assert.Equal(t, rendered, messages[5].Content)
	assert.Len(t, renderedSkills(messages), 1)
}

func TestCompactionReattachesModelSkillWithoutOrphanedToolResult(t *testing.T) {
	mockLLM := &compactionMockLLM{response: &llmwire.Response{
		Text: "## Goal\nReview\n## Progress\n- Started\n## Context for Continuation\nContinue",
	}}
	s := newCompactionTestSvc(mockLLM)
	rendered := builtin.RenderSkill(&loader.Skill{Name: "review", Content: "Review carefully."}, "")

	s.ms.setMessages([]llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		{Role: llmwire.RoleUser, Content: "task"},
		{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "skill-1", Name: "skill"}}},
		{Role: llmwire.RoleTool, Content: "[review]\n" + rendered, ToolCallID: "skill-1", ToolName: "skill"},
		{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "c1", Name: "read"}}},
		{Role: llmwire.RoleTool, Content: "one", ToolCallID: "c1", ToolName: "read"},
		{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "c2", Name: "read"}}},
		{Role: llmwire.RoleTool, Content: "two", ToolCallID: "c2", ToolName: "read"},
	})

	err := s.compactIfNeeded(context.Background(), 1, 1)
	require.NoError(t, err)

	skills := renderedSkills(s.ms.getMessages())
	require.Len(t, skills, 1)
	assert.Equal(t, llmwire.RoleUser, skills[0].Role)
	assert.Empty(t, skills[0].ToolCallID)
	assert.Empty(t, skills[0].ToolName)
}

func TestCompactionUsesProductionHeaderWithoutAgentsMD(t *testing.T) {
	mockLLM := &compactionMockLLM{response: &llmwire.Response{
		Text: "## Goal\nReview\n## Progress\n- Started\n## Context for Continuation\nContinue",
	}}
	s := newCompactionTestSvc(mockLLM)
	rendered := builtin.RenderSkill(&loader.Skill{Name: "review", Content: "Review carefully."}, "")

	s.ms.setMessages([]llmwire.Message{
		{Role: llmwire.RoleUser, Content: "task"},
		{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "skill-1", Name: tool.IDSkill}}},
		{Role: llmwire.RoleTool, Content: rendered, ToolCallID: "skill-1", ToolName: tool.IDSkill},
		{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "c1", Name: "read"}}},
		{Role: llmwire.RoleTool, Content: "one", ToolCallID: "c1", ToolName: "read"},
		{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "c2", Name: "read"}}},
		{Role: llmwire.RoleTool, Content: "two", ToolCallID: "c2", ToolName: "read"},
	})

	require.NoError(t, s.compactIfNeeded(context.Background(), 1, 1))

	messages := s.ms.getMessages()
	assert.Equal(t, "task", messages[0].Content)
	assert.Empty(t, messages[0].ToolCalls)
	require.Len(t, renderedSkills(messages), 1)
	assert.Empty(t, unresolvedToolCalls(messages))
}

func TestSelectSkillReattachmentsKeepsLatestAndRespectsBudgets(t *testing.T) {
	messages := make([]llmwire.Message, 0, 8)
	messages = append(messages,
		llmwire.Message{Role: llmwire.RoleSystem, Content: "sys"},
		llmwire.Message{Role: llmwire.RoleUser, Content: "task"},
	)

	for i := range 6 {
		name := fmt.Sprintf("skill-%d", i)
		content := builtin.RenderSkill(&loader.Skill{
			Name:    name,
			Content: strings.Repeat("界", skillReattachMaxTokens*skillReattachCharsPerToken+100),
		}, "")
		messages = append(messages, llmwire.Message{Role: llmwire.RoleUser, Content: content})
	}

	reattachments := selectSkillReattachments(
		messages,
		compactionHeaderSize(messages),
		len(messages),
		testReattachWindow,
	)

	require.Len(t, reattachments, 5)
	for i, message := range reattachments {
		name, _, ok := builtin.ExtractRenderedSkill(message.Content)
		require.True(t, ok)
		assert.Equal(t, fmt.Sprintf("skill-%d", i+1), name)
		assert.Equal(t, llmwire.RoleUser, message.Role)
		assert.LessOrEqual(t, estimateSkillTokens(message.Content), skillReattachMaxTokens)
		assert.LessOrEqual(t, utf8.RuneCountInString(message.Content), 20000)
		assert.Contains(t, message.Content, "[Skill content truncated during compaction]")
		assert.True(t, utf8.ValidString(message.Content))
		assert.True(t, strings.HasSuffix(message.Content, "</skill>"))
	}
}

func TestSelectSkillReattachmentsFindsEverySkillInBatchHistory(t *testing.T) {
	first := builtin.RenderSkill(&loader.Skill{Name: "first", Content: "First instructions."}, "")
	second := builtin.RenderSkill(&loader.Skill{Name: "second", Content: "Second instructions."}, "")
	messages := []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		{Role: llmwire.RoleUser, Content: "task"},
		{
			Role:     llmwire.RoleTool,
			ToolName: tool.IDBatch,
			Content:  "=== skill (call 1) ===\n" + first + "\n=== skill (call 2) ===\n" + second,
		},
	}

	reattachments := selectSkillReattachments(
		messages,
		compactionHeaderSize(messages),
		len(messages),
		testReattachWindow,
	)

	require.Len(t, reattachments, 2)
	assert.Equal(t, first, reattachments[0].Content)
	assert.Equal(t, second, reattachments[1].Content)
}

func TestEstimateSkillTokensUsesRuneFloorApproximation(t *testing.T) {
	assert.Equal(t, 1, estimateSkillTokens("1234567"))
	assert.Equal(t, 1, estimateSkillTokens("界界界界界界界"))
}

func TestSelectSkillReattachmentsDoesNotDuplicateRetainedLatestInvocation(t *testing.T) {
	old := builtin.RenderSkill(&loader.Skill{Name: "review", Content: "old"}, "")
	latest := builtin.RenderSkill(&loader.Skill{Name: "review", Content: "latest"}, "")
	messages := []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		{Role: llmwire.RoleUser, Content: "task"},
		{Role: llmwire.RoleUser, Content: old},
		{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "c1", Name: "read"}}},
		{Role: llmwire.RoleTool, Content: "one", ToolCallID: "c1", ToolName: "read"},
		{Role: llmwire.RoleUser, Content: latest},
	}

	reattachments := selectSkillReattachments(messages, compactionHeaderSize(messages), 5, testReattachWindow)

	assert.Empty(t, reattachments)
}

func TestIncrementalCompactionKeepsSingleSkillReattachment(t *testing.T) {
	mockLLM := &compactionMockLLM{response: &llmwire.Response{
		Text: "## Goal\nReview\n## Progress\n- Working\n## Context for Continuation\nContinue",
	}}
	s := newCompactionTestSvc(mockLLM)
	rendered := builtin.RenderSkill(&loader.Skill{Name: "review", Content: "Review carefully."}, "")

	s.ms.setMessages([]llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		{Role: llmwire.RoleUser, Content: "task"},
		{Role: llmwire.RoleUser, Content: rendered},
		{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "c1", Name: "read"}}},
		{Role: llmwire.RoleTool, Content: "one", ToolCallID: "c1", ToolName: "read"},
		{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "c2", Name: "read"}}},
		{Role: llmwire.RoleTool, Content: "two", ToolCallID: "c2", ToolName: "read"},
	})

	require.NoError(t, s.compactIfNeeded(context.Background(), 1, 1))
	assert.Len(t, renderedSkills(s.ms.getMessages()), 1)

	s.ms.mu.Lock()
	s.ms.messages = append(s.ms.messages,
		llmwire.Message{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "c3", Name: "read"}}},
		llmwire.Message{Role: llmwire.RoleTool, Content: "three", ToolCallID: "c3", ToolName: "read"},
		llmwire.Message{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "c4", Name: "read"}}},
		llmwire.Message{Role: llmwire.RoleTool, Content: "four", ToolCallID: "c4", ToolName: "read"},
	)
	s.ms.mu.Unlock()

	require.NoError(t, s.compactIfNeeded(context.Background(), 1, 1))
	assert.Len(t, renderedSkills(s.ms.getMessages()), 1)
}

func TestCompactionPersistsSkillReattachmentAcrossReload(t *testing.T) {
	ctx := context.Background()
	store := &compactionRecordingStore{nextID: 1}
	mockLLM := &compactionMockLLM{response: &llmwire.Response{
		Text: "## Goal\nReview\n## Progress\n- Working\n## Context for Continuation\nContinue",
	}}
	s := newCompactionTestSvc(mockLLM)
	s.ms = newMessageStore(store, 1)
	rendered := builtin.RenderSkill(&loader.Skill{Name: "review", Content: "Review carefully."}, "")

	for _, message := range []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		{Role: llmwire.RoleUser, Content: "task"},
		{Role: llmwire.RoleUser, Content: rendered},
		{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "c1", Name: "read"}}},
		{Role: llmwire.RoleTool, Content: "one", ToolCallID: "c1", ToolName: "read"},
		{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "c2", Name: "read"}}},
		{Role: llmwire.RoleTool, Content: "two", ToolCallID: "c2", ToolName: "read"},
	} {
		s.ms.mu.Lock()
		require.NoError(t, s.ms.appendMessageLocked(ctx, &message))
		s.ms.mu.Unlock()
	}

	require.NoError(t, s.compactIfNeeded(ctx, 1, 1))

	reloaded := newMessageStore(store, 1)
	require.NoError(t, reloaded.reloadMessages(ctx))
	reloadedMessages := reloaded.getMessages()
	inMemoryMessages := s.ms.getMessages()
	require.Len(t, reloadedMessages, len(inMemoryMessages))

	for i := range inMemoryMessages {
		assert.Equal(t, inMemoryMessages[i].Role, reloadedMessages[i].Role)
		assert.Equal(t, inMemoryMessages[i].Content, reloadedMessages[i].Content)
		assert.Equal(t, inMemoryMessages[i].ToolCallID, reloadedMessages[i].ToolCallID)
	}

	skills := renderedSkills(reloadedMessages)
	require.Len(t, skills, 1)
	assert.Equal(t, llmwire.RoleUser, skills[0].Role)
	assert.Equal(t, rendered, skills[0].Content)
}

func TestValidateSummary(t *testing.T) {
	ok, missing := validateSummary("## Goal\nFix\n## Progress\n- Done\n## Context for Continuation\nContinue")
	assert.True(t, ok)
	assert.Empty(t, missing)

	ok, missing = validateSummary("## Goal\nFix")
	assert.False(t, ok)
	assert.Contains(t, missing, "## Progress")
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

// makeTestMessages creates a conversation with N rounds.
func makeTestMessages(nRounds int) []llmwire.Message {
	msgs := make([]llmwire.Message, 0, 2+2*nRounds)
	msgs = append(msgs,
		llmwire.Message{Role: llmwire.RoleUser, Content: agentsMDMessagePrefix + "project rules"},
		llmwire.Message{Role: llmwire.RoleUser, Content: "Fix the authentication bug in login.go"},
	)

	filler := strings.Repeat("x", 1000)

	for i := range nRounds {
		callID := fmt.Sprintf("call_%d", i)
		msgs = append(msgs, llmwire.Message{
			Role:    llmwire.RoleAssistant,
			Content: fmt.Sprintf("Working on step %d", i),
			ToolCalls: []llmwire.ToolCall{
				{ID: callID, Name: "read", Arguments: fmt.Appendf(nil, `{"path":"file%d.go"}`, i)},
			},
		})
		msgs = append(msgs, llmwire.Message{
			Role:       llmwire.RoleTool,
			Content:    fmt.Sprintf("Content of file%d.go: %s", i, filler),
			ToolCallID: callID,
			ToolName:   "read",
		})
	}
	return msgs
}

func TestCompaction_PostCompactionMessageStructure(t *testing.T) {
	validSummary := `## Goal
Fix the authentication bug in login.go

## Progress
- Read multiple files

## Context for Continuation
Continue with the fix`

	mockLLM := &compactionMockLLM{response: &llmwire.Response{Text: validSummary}}
	s := newCompactionTestSvc(mockLLM)
	s.ms.setMessages(makeTestMessages(20))

	compacted, err := s.compact(context.Background(), 6)
	require.NoError(t, err)
	assert.True(t, compacted)

	// header → summary → ack → primer → (reattachments) → nothing.
	msgs := s.ms.getMessages()
	require.Len(t, msgs, 5)

	assert.Equal(t, llmwire.RoleUser, msgs[0].Role)
	assert.Equal(t, llmwire.RoleUser, msgs[1].Role)
	assert.Equal(t, llmwire.RoleUser, msgs[2].Role)
	assert.True(t, strings.HasPrefix(msgs[2].Content, "[CONTEXT SUMMARY"))
	assert.Equal(t, llmwire.RoleAssistant, msgs[3].Role)
	assert.Equal(t, registry.PostCompactionAssistantAck, msgs[3].Content)
	assert.Equal(t, llmwire.RoleUser, msgs[4].Role)
	assert.Contains(t, msgs[4].Content, "[Post-compaction context refresh]")
}

func TestCompactionPrompts_IdentifierPreservation(t *testing.T) {
	assert.Contains(t, registry.CompactionInitialPrompt, "identifiers exactly as written")
	assert.Contains(t, registry.CompactionMergePrompt, "MUST PRESERVE")
}
