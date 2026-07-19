package session

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/tool"
)

// MarkCleared stamps cleared_at on the recorded rows without touching their
// stored content — the append-only contract the render path relies on.
func (s *compactionRecordingStore) MarkCleared(_ context.Context, ids []int64) error {
	if s.markClearedErr != nil {
		return s.markClearedErr
	}

	now := time.Now()
	set := make(map[int64]bool, len(ids))

	for _, id := range ids {
		set[id] = true
	}

	for _, message := range s.messages {
		if set[message.ID] {
			message.ClearedAt = &now
		}
	}

	return nil
}

const skillEnvelope = "<skill>\n<name>demo</name>\n---\nbody\n</skill>"

type renderedMsg struct {
	Role       string
	Content    string
	ToolCallID string
	ToolName   string
}

func renderView(msgs []llmwire.Message) []renderedMsg {
	out := make([]renderedMsg, len(msgs))
	for i, m := range msgs {
		out[i] = renderedMsg{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID, ToolName: m.ToolName}
	}

	return out
}

func TestClearEligible(t *testing.T) {
	tests := []struct {
		name     string
		msg      llmwire.Message
		external map[string]bool
		want     bool
	}{
		{"plain read tool", llmwire.Message{Role: llmwire.RoleTool, ToolName: "read"}, nil, true},
		{"grep tool", llmwire.Message{Role: llmwire.RoleTool, ToolName: "grep"}, nil, true},
		{"skill envelope kept", llmwire.Message{Role: llmwire.RoleTool, ToolName: tool.IDSkill}, nil, false},
		{
			"batch with rendered skill kept",
			llmwire.Message{Role: llmwire.RoleTool, ToolName: tool.IDBatch, Content: skillEnvelope},
			nil,
			false,
		},
		{
			"batch without skill cleared",
			llmwire.Message{Role: llmwire.RoleTool, ToolName: tool.IDBatch, Content: "plain output"},
			nil,
			true,
		},
		{
			"pending external kept",
			llmwire.Message{Role: llmwire.RoleTool, ToolName: "sleep", ToolCallID: "s1"},
			map[string]bool{"s1": true},
			false,
		},
		{"assistant untouched", llmwire.Message{Role: llmwire.RoleAssistant, Content: "hi"}, nil, false},
		{"user untouched", llmwire.Message{Role: llmwire.RoleUser, Content: "task"}, nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, clearEligible(tt.msg, tt.external))
		})
	}
}

// TestApplyClear_ExclusionMatrix drives applyClear over a transcript covering
// every Decision 5 case and asserts exactly which tool results are replaced.
func TestApplyClear_ExclusionMatrix(t *testing.T) {
	s := newCompactionTestSvc(&compactionMockLLM{})

	asst := func(id, name string) llmwire.Message {
		return llmwire.Message{
			Role:      llmwire.RoleAssistant,
			Content:   "step",
			ToolCalls: []llmwire.ToolCall{{ID: id, Name: name}},
		}
	}
	res := func(id, name, content string) llmwire.Message {
		return llmwire.Message{Role: llmwire.RoleTool, Content: content, ToolCallID: id, ToolName: name}
	}

	s.ms.setMessages([]llmwire.Message{
		{Role: llmwire.RoleUser, Content: "task"},   // 0
		asst("c1", "read"),                          // 1
		res("c1", "read", "[/a.go]\nAAA"),           // 2 → cleared
		asst("c2", "grep"),                          // 3
		res("c2", "grep", "matches BBB"),            // 4 → cleared
		asst("c3", tool.IDSkill),                    // 5
		res("c3", tool.IDSkill, skillEnvelope),      // 6 → kept (skill)
		asst("c4", tool.IDBatch),                    // 7
		res("c4", tool.IDBatch, skillEnvelope),      // 8 → kept (batch w/ skill)
		asst("c5", tool.IDBatch),                    // 9
		res("c5", tool.IDBatch, "plain CCC output"), // 10 → cleared (batch no skill)
		asst("c6", "read"),                          // 11 protected tail
		res("c6", "read", "[/b.go]\nDDD"),           // 12 protected
		asst("c7", "read"),                          // 13 protected tail
		res("c7", "read", "[/c.go]\nEEE"),           // 14 protected
	})

	n := s.applyClear(context.Background(), 2) // keep last 2 rounds
	require.Equal(t, 3, n)

	msgs := s.ms.getMessages()

	assert.Equal(t, clearedPlaceholder("read"), msgs[2].Content, "old read cleared")
	assert.Equal(t, clearedPlaceholder("grep"), msgs[4].Content, "old grep cleared")
	assert.Equal(t, clearedPlaceholder(tool.IDBatch), msgs[10].Content, "old skill-free batch cleared")

	assert.Equal(t, skillEnvelope, msgs[6].Content, "skill result kept")
	assert.Equal(t, skillEnvelope, msgs[8].Content, "batch-with-skill kept")
	assert.Equal(t, "[/b.go]\nDDD", msgs[12].Content, "protected tail kept")
	assert.Equal(t, "[/c.go]\nEEE", msgs[14].Content, "protected tail kept")

	assert.Equal(t, "step", msgs[1].Content, "assistant rows untouched")
	assert.Equal(t, "task", msgs[0].Content, "user row untouched")

	// Second apply with the same params is a no-op.
	assert.Equal(t, 0, s.applyClear(context.Background(), 2))
}

// TestApplyClear_ByteStability asserts the rendered view is identical whether it
// comes straight from the event apply or from a fresh reload of the store.
func TestApplyClear_ByteStability(t *testing.T) {
	store := &compactionRecordingStore{nextID: 1}
	s := &svc{
		llmClient:    &compactionMockLLM{},
		ms:           newMessageStore(store, 1),
		loopDetector: newLoopDetector(),
		registry:     tool.NewRegistry(),
		prompt:       newPromptBuilder(testPrompt, "", ""),
	}

	ctx := context.Background()
	for i := range 4 {
		cid := fmt.Sprintf("c%d", i)
		require.NoError(
			t,
			s.ms.addAssistantMessage(
				ctx,
				&llmwire.Response{Text: "step", ToolCalls: []llmwire.ToolCall{{ID: cid, Name: "read"}}},
			),
		)
		require.NoError(t, s.ms.addToolResult(ctx, cid, "read", fmt.Sprintf("[/f%d.go]\nbody-%d", i, i)))
	}

	require.Positive(t, s.applyClear(ctx, 1))

	afterApply := renderView(s.ms.getMessages())

	require.NoError(t, s.ms.reloadMessages(ctx))
	afterReload := renderView(s.ms.getMessages())

	assert.Equal(t, afterApply, afterReload, "event-apply and reload must render identically")

	// A second apply with the same params changes nothing.
	assert.Equal(t, 0, s.applyClear(ctx, 1))
	assert.Equal(t, afterApply, renderView(s.ms.getMessages()))
}

// TestApplyClear_PreservesStoredContent verifies clearing is metadata-only: the
// in-memory view shows a placeholder but the persisted row keeps its content.
func TestApplyClear_PreservesStoredContent(t *testing.T) {
	store := &compactionRecordingStore{nextID: 1}
	s := &svc{
		llmClient:    &compactionMockLLM{},
		ms:           newMessageStore(store, 1),
		loopDetector: newLoopDetector(),
		registry:     tool.NewRegistry(),
		prompt:       newPromptBuilder(testPrompt, "", ""),
	}

	ctx := context.Background()
	for i := range 3 {
		cid := fmt.Sprintf("c%d", i)
		require.NoError(
			t,
			s.ms.addAssistantMessage(
				ctx,
				&llmwire.Response{Text: "step", ToolCalls: []llmwire.ToolCall{{ID: cid, Name: "read"}}},
			),
		)
		require.NoError(t, s.ms.addToolResult(ctx, cid, "read", fmt.Sprintf("original-%d", i)))
	}

	require.Positive(t, s.applyClear(ctx, 1))
	require.NoError(t, s.ms.reloadMessages(ctx))

	var placeholders, originalsInStore, clearedRows int

	for _, m := range s.ms.getMessages() {
		if m.Content == clearedPlaceholder("read") {
			placeholders++
		}
	}

	for _, sm := range store.messages {
		if strings.HasPrefix(sm.Content, "original-") {
			originalsInStore++

			if sm.ClearedAt != nil {
				clearedRows++
			}
		}
	}

	assert.Positive(t, placeholders, "at least one placeholder rendered after reload")
	assert.Equal(t, 3, originalsInStore, "all original tool-result content survives in the DB")
	assert.Equal(
		t,
		placeholders,
		clearedRows,
		"every rendered placeholder maps to a cleared_at row with intact content",
	)
}

// TestApplyClear_PersistFailureLeavesViewsConsistent asserts that a failed
// MarkCleared is a no-op: nothing is substituted in memory (so the next reload
// can't silently revert a placeholder), and the count is 0.
func TestApplyClear_PersistFailureLeavesViewsConsistent(t *testing.T) {
	store := &compactionRecordingStore{nextID: 1, markClearedErr: assert.AnError}
	s := &svc{
		llmClient:    &compactionMockLLM{},
		ms:           newMessageStore(store, 1),
		loopDetector: newLoopDetector(),
		registry:     tool.NewRegistry(),
		prompt:       newPromptBuilder(testPrompt, "", ""),
	}

	ctx := context.Background()
	for i := range 3 {
		cid := fmt.Sprintf("c%d", i)
		require.NoError(
			t,
			s.ms.addAssistantMessage(
				ctx,
				&llmwire.Response{Text: "step", ToolCalls: []llmwire.ToolCall{{ID: cid, Name: "read"}}},
			),
		)
		require.NoError(t, s.ms.addToolResult(ctx, cid, "read", fmt.Sprintf("original-%d", i)))
	}

	assert.Equal(t, 0, s.applyClear(ctx, 1), "failed persist reports nothing cleared")

	for _, m := range s.ms.getMessages() {
		if m.Role == llmwire.RoleTool {
			assert.NotEqual(t, clearedPlaceholder("read"), m.Content, "no in-memory placeholder on persist failure")
		}
	}
}

func contextEventRunner(s *svc, notes *[]string) *loopRunner {
	return &loopRunner{
		agent: s,
		opts: loopOptions{Notify: func(_ context.Context, m string) error {
			*notes = append(*notes, m)
			return nil
		}},
		log:    zap.NewNop(),
		result: &loopResult{},
	}
}

func bigRounds(n, contentSize int) []llmwire.Message {
	msgs := make([]llmwire.Message, 0, 1+2*n)
	msgs = append(msgs, llmwire.Message{Role: llmwire.RoleUser, Content: "task"})
	body := strings.Repeat("x", contentSize)

	for i := range n {
		cid := fmt.Sprintf("c%d", i)
		msgs = append(
			msgs,
			llmwire.Message{
				Role:      llmwire.RoleAssistant,
				Content:   "s",
				ToolCalls: []llmwire.ToolCall{{ID: cid, Name: "read"}},
			},
			llmwire.Message{Role: llmwire.RoleTool, Content: body, ToolCallID: cid, ToolName: "read"},
		)
	}

	return msgs
}

func notesContain(notes []string, sub string) bool {
	for _, n := range notes {
		if strings.Contains(n, sub) {
			return true
		}
	}

	return false
}

// Crossing the threshold goes straight to compaction: clearing is the
// compactor's own first phase, never a separate event that can spare it.
func TestApplyContextEvents_ThresholdCompactsWithoutASeparateClearStage(t *testing.T) {
	summary := &llmwire.Response{
		Text: "## Goal\nx\n## Progress\n- y\n## Context for Continuation\nz",
	}

	t.Run("clearing alone can no longer avert summarization", func(t *testing.T) {
		llm := &compactionMockLLM{response: summary, contextWindow: 40000}
		s := newCompactionTestSvc(llm)

		// Rounds 0-1 huge (outside the keep-6 tail → cleared), rounds 2-7 small:
		// clearing alone would drop the transcript back under the threshold.
		msgs := bigRounds(2, 80000)
		msgs = append(msgs[:len(msgs):len(msgs)], bigRounds(6, 40)[1:]...) // drop the extra "task" user msg
		s.ms.setMessages(msgs)

		var notes []string
		contextEventRunner(s, &notes).applyContextEvents(context.Background())

		assert.Equal(t, 1, llm.callCount, "the threshold is answered by compaction, nothing else")
		assert.False(t, notesContain(notes, "🧹 Cleared"), "clearing is not an event of its own any more")
		assert.True(t, notesContain(notes, "Compacting"))
	})

	t.Run("below the threshold nothing happens", func(t *testing.T) {
		llm := &compactionMockLLM{response: summary, contextWindow: 200000}
		s := newCompactionTestSvc(llm)
		s.ms.setMessages(bigRounds(8, 40))

		var notes []string
		contextEventRunner(s, &notes).applyContextEvents(context.Background())

		assert.Equal(t, 0, llm.callCount)
		assert.Empty(t, notes, "an untouched transcript notifies nothing")
	})

	t.Run("explicit compaction forces regardless of the threshold", func(t *testing.T) {
		llm := &compactionMockLLM{response: summary, contextWindow: 400000}
		s := newCompactionTestSvc(llm)

		s.ms.setMessages(bigRounds(8, 20000))
		s.RequestCompaction(2)

		var notes []string
		contextEventRunner(s, &notes).applyContextEvents(context.Background())

		assert.Positive(t, llm.callCount, "explicit compaction summarizes")
		assert.True(t, notesContain(notes, "✅ Context compacted"))
	})
}
