package session

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
)

// pageScan builds one user message carrying one measured page-scan attachment
// (1615×2193 px, the incident's shape).
func pageScan(id string, rawBytes int64) llmwire.Message {
	return llmwire.Message{
		Role:    llmwire.RoleUser,
		Content: "page scan",
		Images: []llmwire.ImageRef{{
			Path: "/tmp/" + id + ".png", Mime: llmwire.MimeImagePng, Size: rawBytes,
			Width: 1615, Height: 2193,
		}},
	}
}

func TestImageBase64Bytes(t *testing.T) {
	assert.Equal(t, int64(0), imageBase64Bytes(0))
	assert.Equal(t, int64(4), imageBase64Bytes(1), "ceil(size/3)*4")
	assert.Equal(t, int64(4), imageBase64Bytes(3))
	assert.Equal(t, int64(8), imageBase64Bytes(4))
}

func TestShouldCompactImageBytePressure(t *testing.T) {
	const window = 1_048_576

	// The incident shape: 40 page scans, ~122 MB base64 at ~18% token
	// occupancy — the token projection sat far below the trigger, and the
	// count axis was over its mark too.
	incident := []llmwire.Message{{Role: llmwire.RoleSystem, Content: "sys"}}
	for i := range 40 {
		incident = append(incident, pageScan(fmt.Sprintf("p%d", i), 2_290_000))
	}

	agent := newTestAgent()
	agent.ms.setMessages(incident)

	_, count := imagePressure(incident)
	assert.Greater(t, count, imageCountHighWater)
	assert.True(t, agent.shouldCompact(window), "the byte wall triggers compaction")

	// The byte axis fires on its own: 15 oversized scans breach 12 MB while
	// the count axis stays quiet.
	byteOnly := []llmwire.Message{{Role: llmwire.RoleSystem, Content: "sys"}}
	for i := range 15 {
		byteOnly = append(byteOnly, pageScan(fmt.Sprintf("b%d", i), 5_000_000))
	}

	agent.ms.setMessages(byteOnly)

	bytes, count := imagePressure(byteOnly)
	assert.Greater(t, bytes, int64(imageBytesHighWater))
	assert.LessOrEqual(t, count, imageCountHighWater)

	size, _ := agent.projectContextSize()
	assert.Less(t, size, compactionCutoff(window), "the token axis alone stays quiet")
	assert.True(t, agent.shouldCompact(window))
}

func TestShouldCompactImageCountPressure(t *testing.T) {
	// 21 tiny crops pass any byte budget and still break the count limit (D5).
	transcript := []llmwire.Message{{Role: llmwire.RoleSystem, Content: "sys"}}
	for i := range 21 {
		transcript = append(transcript, pageScan(fmt.Sprintf("c%d", i), 3_000))
	}

	agent := newTestAgent()
	agent.ms.setMessages(transcript)

	bytes, count := imagePressure(transcript)
	assert.Less(t, bytes, int64(imageBytesHighWater))
	assert.Greater(t, count, imageCountHighWater)
	assert.True(t, agent.shouldCompact(1_048_576))

	// Equality is not a breach: 20 crops stay quiet, on either axis.
	agent.ms.setMessages(transcript[:21])
	_, count = imagePressure(transcript[:21])
	assert.Equal(t, imageCountHighWater, count)
	assert.False(t, agent.shouldCompact(1_048_576))
}

func TestShouldCompactFourScansStayQuiet(t *testing.T) {
	transcript := []llmwire.Message{{Role: llmwire.RoleSystem, Content: "sys"}}
	for i := range 4 {
		transcript = append(transcript, pageScan(fmt.Sprintf("p%d", i), 2_000_000))
	}

	agent := newTestAgent()
	agent.ms.setMessages(transcript)

	bytes, _ := imagePressure(transcript)
	assert.Less(t, bytes, int64(imageBytesHighWater), "the fixture total is what matters, not its count")
	assert.False(t, agent.shouldCompact(1_048_576))
}

func TestSelectCheckpointSplitNeverSummarizesWholeShortHistory(t *testing.T) {
	// A short raw history (under window/10) used to be summarized whole: the
	// empty tail was legal. One legal group must stay verbatim (D3).
	messages := []llmwire.Message{
		{Role: llmwire.RoleUser, Content: strings.Repeat("a", 80_000)}, // ~20k tokens
		{Role: llmwire.RoleAssistant, Content: "a", ToolCalls: []llmwire.ToolCall{{ID: "c1", Name: "read"}}},
		{Role: llmwire.RoleTool, Content: "t", ToolCallID: "c1", ToolName: "read"},
	}

	const window = 80_000

	split, ok := selectCheckpointSplit(messages, checkpointPrefix{rawStart: 0}, 0, window)
	require.True(t, ok)
	assert.Less(t, split, len(messages), "the tail is never empty")
	assert.Positive(t, split)
}

func TestSelectCheckpointSplitImageTailCeiling(t *testing.T) {
	const window = 1_048_576

	// Six page scans: the token floor wants half the history verbatim, but two
	// scans already push the tail over the 6 MB ceiling — the ceiling wins (D2).
	messages := []llmwire.Message{{Role: llmwire.RoleUser, Content: "task"}}
	for i := range 6 {
		messages = append(messages,
			llmwire.Message{
				Role:      llmwire.RoleAssistant,
				Content:   "a",
				ToolCalls: []llmwire.ToolCall{{ID: fmt.Sprintf("c%d", i), Name: "read"}},
			},
			llmwire.Message{
				Role: llmwire.RoleTool, Content: "t", ToolCallID: fmt.Sprintf("c%d", i), ToolName: "read",
				Images: []llmwire.ImageRef{{
					Path: fmt.Sprintf("/tmp/p%d.png", i), Mime: llmwire.MimeImagePng, Size: 2_290_000,
					Width: 1615, Height: 2193,
				}},
			},
		)
	}

	split, ok := selectCheckpointSplit(messages, checkpointPrefix{rawStart: 0}, 0, window)
	require.True(t, ok)

	tail := messages[split:]
	tailBytes, tailCount := imagePressure(tail)
	assert.LessOrEqual(t, tailBytes, int64(imageBytesLowWater))
	assert.LessOrEqual(t, tailCount, imageCountLowWater)
	assert.NotEmpty(t, tail, "the tail is never empty")
	assert.Less(t, split, len(messages))
}

// The whole automatic path: byte pressure triggers, the checkpoint relieves
// both axes, and a verbatim tail survives under the ceilings.
func TestImagePressureCompactionRelievesTheByteAxis(t *testing.T) {
	ctx := context.Background()

	mockLLM := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: 1_048_576,
	}
	s := newCompactionTestSvc(mockLLM)

	messages := []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		{Role: llmwire.RoleUser, Content: "scan the archive"},
	}
	for i := range 12 {
		messages = append(messages,
			llmwire.Message{
				Role:      llmwire.RoleAssistant,
				Content:   "reading",
				ToolCalls: []llmwire.ToolCall{{ID: fmt.Sprintf("c%d", i), Name: "read"}},
			},
			llmwire.Message{
				Role: llmwire.RoleTool, Content: "page", ToolCallID: fmt.Sprintf("c%d", i), ToolName: "read",
				Images: []llmwire.ImageRef{{
					Path: fmt.Sprintf("/tmp/p%d.png", i), Mime: llmwire.MimeImagePng, Size: 2_290_000,
					Width: 1615, Height: 2193,
				}},
			},
		)
	}
	s.ms.setMessages(messages)

	require.True(t, s.shouldCompact(1_048_576), "byte pressure fires the trigger")

	ok, err := s.compact(ctx, nil)
	require.NoError(t, err)
	require.True(t, ok)

	after := s.ms.getMessages()
	afterBytes, afterCount := imagePressure(after)
	assert.LessOrEqual(t, afterBytes, int64(imageBytesHighWater), "the checkpoint relieves the byte axis")
	assert.LessOrEqual(t, afterCount, imageCountHighWater)
	assert.False(t, s.shouldCompact(1_048_576), "the session is out of pressure")

	summaryIdx := -1
	for i, m := range after {
		if isMarkedSummary(m.Content) {
			summaryIdx = i
			break
		}
	}
	require.Positive(t, summaryIdx, "a summary row was written")

	// The tail is verbatim, non-empty, and holds the remaining pixels.
	tail := after[summaryIdx+1:]
	assert.NotEmpty(t, tail)
	tailBytes, _ := imagePressure(tail)
	assert.LessOrEqual(t, tailBytes, int64(imageBytesLowWater))
}
