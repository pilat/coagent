package session

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
)

// The selected tail is the existing rows verbatim: same row IDs, same bytes, same
// order — never copies.
func TestCheckpointRetainsTheTailVerbatim(t *testing.T) {
	const window = 32000

	ctx := context.Background()
	store := &compactionRecordingStore{nextID: 1}
	llm := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: window,
	}
	s := newCompactionTestSvc(llm)
	s.ms = newMessageStore(store, 1)

	original := oversizedTranscript(window)
	for i := range original {
		message := original[i]
		s.ms.mu.Lock()
		require.NoError(t, s.ms.appendMessageLocked(ctx, &message))
		s.ms.mu.Unlock()
	}
	before := s.ms.getMessages()
	beforeRowIDs := s.ms.getRowIDs()

	require.NoError(t, s.compactIfNeeded(ctx, window))

	after := s.ms.getMessages()
	afterRowIDs := s.ms.getRowIDs()

	// The tail is a suffix of the original: find the marked summary row and
	// compare everything after it byte-for-byte, row IDs included.
	splitAt := -1

	for i, m := range after {
		if isMarkedSummary(m.Content) {
			splitAt = i + 1

			break
		}
	}

	require.Positive(t, splitAt, "a checkpoint was committed")
	require.Less(t, splitAt, len(after), "the tail is non-empty at this window size")

	for i := splitAt; i < len(after); i++ {
		originalOffset := len(before) - (len(after) - i)
		require.GreaterOrEqual(t, originalOffset, splitAt)

		assert.Equal(t, before[originalOffset], after[i],
			"tail row %d is the original row byte-for-byte", i)
		assert.Equal(t, beforeRowIDs[originalOffset], afterRowIDs[i],
			"tail row %d keeps its durable identity", i)
	}
}

// Restart after one and two checkpoints derives the exact prior summary from the
// marked message itself — no session metadata is involved.
func TestRestartDerivesTheAnchorFromTheMarkedSummaryRow(t *testing.T) {
	const window = 32000

	llm := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: window,
	}
	s := newCompactionTestSvc(llm)
	s.ms.setMessages(oversizedTranscript(window))

	require.NoError(t, s.compactIfNeeded(context.Background(), window))

	reloaded := newCompactionTestSvc(llm)
	reloaded.ms.setMessages(s.ms.getMessages())

	cp := parseCheckpointPrefix(reloaded.ms.getMessages(), compactionHeaderSize(reloaded.ms.getMessages()))
	require.NotEqual(t, -1, cp.summaryRowIdx, "the marked summary row follows the header after a reload")
	assert.Equal(t, validSummary, cp.prevSummary, "the anchor is the extracted model text")

	// A malformed wrapper in the scaffolding position is ordinary history: it
	// neither becomes an anchor nor hides the raw rows behind it.
	tampered := s.ms.getMessages()
	tampered[2] = compactionUserMessage(compactionMarkOpen + "\n\nbroken checkpoint without a close")

	cp = parseCheckpointPrefix(tampered, compactionHeaderSize(tampered))
	assert.Equal(t, -1, cp.summaryRowIdx, "an incomplete wrapper is ordinary history")
}

// After the winning completion CAS, a producer row committed outside the
// compaction snapshot loads after the selected tail, whichever reload runs
// first — and exactly once.
func TestOutsideSnapshotCompletionLoadsAfterTheTailInBothReloadOrders(t *testing.T) {
	const window = 32000

	ctx := context.Background()

	for _, order := range []string{"completion-reload-then-loop-reload", "loop-reload-then-completion-reload"} {
		t.Run(order, func(t *testing.T) {
			store := &compactionRecordingStore{nextID: 1}
			llm := &compactionMockLLM{
				response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
				contextWindow: window,
			}
			s := newCompactionTestSvc(llm)
			s.ms = newMessageStore(store, 1)

			// Persist the transcript so the store owns the rows the loop reads.
			for i := range oversizedTranscript(window) {
				message := oversizedTranscript(window)[i]
				s.ms.mu.Lock()
				require.NoError(t, s.ms.appendMessageLocked(ctx, &message))
				s.ms.mu.Unlock()
			}

			require.NoError(t, s.compactIfNeeded(ctx, window))
			require.NoError(t, s.ms.reloadMessages(ctx))
			afterCompaction := s.ms.getMessages()
			afterCompactionRowIDs := s.ms.getRowIDs()
			require.True(t, hasSummaryRow(afterCompaction))

			// The completion is committed by the store while the parent may hold
			// a stale candidate: insertMessageWith leaves its position NULL, so
			// LoadActiveMessages sorts it after the positioned tail.
			callID := fmt.Sprintf("child-%s", order)
			asstMem := mkStoredAssistant(callID)
			resultMem := mkStoredResult(callID)
			asstStored, err := storedMessage(&asstMem)
			require.NoError(t, err)
			resultStored, err := storedMessage(&resultMem)
			require.NoError(t, err)
			asstID, err := store.InsertMessage(ctx, 1, asstStored)
			require.NoError(t, err)
			resultID, err := store.InsertMessage(ctx, 1, resultStored)
			require.NoError(t, err)

			if order == "completion-reload-then-loop-reload" {
				require.NoError(t, s.ms.reloadMessages(ctx))
				require.NoError(t, s.ms.reloadMessages(ctx))
			} else {
				require.NoError(t, s.ms.reloadMessages(ctx))
			}

			final := s.ms.getMessages()
			finalRowIDs := s.ms.getRowIDs()

			assert.Equal(t, 1, countRowID(finalRowIDs, asstID), "one in-memory copy of the completion call")
			assert.Equal(t, 1, countRowID(finalRowIDs, resultID), "one in-memory copy of the completion result")

			for _, rowID := range afterCompactionRowIDs {
				assert.Equal(t, 1, countRowID(finalRowIDs, rowID),
					"checkpoint row %d appears exactly once after either reload order", rowID)
			}

			// The outside-snapshot rows sort after the selected tail.
			summaryAt := -1
			for i, m := range final {
				if isMarkedSummary(m.Content) {
					summaryAt = i
				}
			}

			require.Positive(t, summaryAt)
			assert.Greater(t, indexWithRowID(finalRowIDs, asstID), summaryAt,
				"the completion lands after the marked summary and the tail")

			// Both reload orders reach the same projection.
			if order == "loop-reload-then-completion-reload" {
				require.NoError(t, s.ms.reloadMessages(ctx))
				assert.Equal(t, final, s.ms.getMessages())
				assert.Equal(t, finalRowIDs, s.ms.getRowIDs())
			}
		})
	}
}

func mkStoredAssistant(callID string) llmwire.Message {
	return llmwire.Message{
		Role:      llmwire.RoleAssistant,
		ToolCalls: []llmwire.ToolCall{{ID: callID, Name: "subagent_event", Arguments: []byte(`{}`)}},
	}
}

func mkStoredResult(callID string) llmwire.Message {
	return llmwire.Message{
		Role: llmwire.RoleTool, ToolCallID: callID, ToolName: "subagent_event", Content: "child done",
	}
}

func countRowID(rowIDs []int64, id int64) int {
	count := 0

	for _, rowID := range rowIDs {
		if rowID == id {
			count++
		}
	}

	return count
}

func indexWithRowID(rowIDs []int64, id int64) int {
	for i, rowID := range rowIDs {
		if rowID == id {
			return i
		}
	}

	return -1
}
