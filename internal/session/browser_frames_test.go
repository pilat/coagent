package session

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/transcript"
)

func browserCall(id string) llmwire.Message {
	return llmwire.Message{
		Role:             llmwire.RoleAssistant,
		Content:          "thinking",
		ReasoningContent: "reason",
		ReasoningRaw:     json.RawMessage(`{"x":1}`),
		ToolCalls: []llmwire.ToolCall{
			{ID: id, Name: tool.PlaywrightToolPrefix + "click", Arguments: []byte(`{"task_state":"next"}`)},
		},
	}
}

func browserFrame(id string, failed bool) llmwire.Message {
	return llmwire.Message{
		Role:       llmwire.RoleTool,
		ToolCallID: id,
		ToolName:   tool.PlaywrightToolPrefix + "click",
		ToolError:  failed,
		Content:    "frame-" + id,
		Images:     []llmwire.ImageRef{{Path: "/frame/" + id, Mime: llmwire.MimeImagePng, Size: 1000}},
	}
}

func TestProjectBrowserFramesLaterSuccessOnly(t *testing.T) {
	input := []llmwire.Message{
		browserCall("one"),
		browserFrame("one", false),
		browserCall("two"),
		browserFrame("two", true),
	}
	failed := projectBrowserFrames(input)
	assert.Equal(t, "frame-one", failed[1].Content)
	assert.Len(t, failed[1].Images, 1)
	input = append(input, browserCall("three"), browserFrame("three", false))
	projected := projectBrowserFrames(input)
	for _, i := range []int{1, 3} {
		assert.Equal(t, supersededBrowserFrame, projected[i].Content)
		assert.Nil(t, projected[i].Images)
	}
	assert.Equal(t, "frame-three", projected[5].Content)
	assert.Equal(t, input[0], projected[0])
	assert.Equal(t, projected, projectBrowserFrames(input))
	assert.Less(t, estimateTokens(projected), estimateTokens(input))
	_, count := imagePressure(projected)
	assert.Equal(t, 1, count)
	entries, err := compactionEntries(projected, []int64{1, 2, 3, 4, 5, 6})
	require.NoError(t, err)
	for _, entry := range entries {
		assert.NotZero(t, entry.ExistingID)
		assert.Nil(t, entry.Message)
	}
}

func TestProjectBrowserFramesSameTurnAndOtherTools(t *testing.T) {
	first := browserCall("one")
	first.ToolCalls = append(first.ToolCalls, llmwire.ToolCall{ID: "two", Name: tool.PlaywrightToolPrefix + "snapshot"})
	input := []llmwire.Message{
		first, browserFrame("one", false), browserFrame("two", false),
		{Role: llmwire.RoleTool, ToolName: "read", Content: "other"},
	}
	projected := projectBrowserFrames(input)
	assert.Equal(t, input, projected)
	input = append(input, browserCall("three"), browserFrame("three", false))
	projected = projectBrowserFrames(input)
	assert.Equal(t, supersededBrowserFrame, projected[1].Content)
	assert.Equal(t, supersededBrowserFrame, projected[2].Content)
	assert.Equal(t, "other", projected[3].Content)
}

func TestFingerprintResultIncludesImageDigest(t *testing.T) {
	first := llmwire.ImageRef{Digest: "aaa"}
	second := llmwire.ImageRef{Digest: "bbb"}
	assert.NotEqual(t, fingerprintResult("same", first), fingerprintResult("same", second))
	assert.NotEqual(t, fingerprintResult("same", first), fingerprintResult("same"))
	assert.NotEqual(t, fingerprintResult("same"), fingerprintResult("different"))
}

func TestBrowserLoopDiversityUsesImageResults(t *testing.T) {
	diverse := newLoopDetector()
	repeating := newLoopDetector()
	for i := range loopDetectorMinFill {
		args, err := json.Marshal(map[string]any{"task_state": i, "direction": "down"})
		require.NoError(t, err)
		diverse.record(
			[]toolRecord{
				{
					name:       "scroll",
					argsHash:   fingerprintArgs(args),
					resultHash: fingerprintResult("same", llmwire.ImageRef{Digest: string(rune('a' + i))}),
				},
			},
		)
		repeating.record(
			[]toolRecord{
				{
					name:       "scroll",
					argsHash:   fingerprintArgs(args),
					resultHash: fingerprintResult("same", llmwire.ImageRef{Digest: string(rune('a' + i%3))}),
				},
			},
		)
	}
	assert.Equal(t, actionNone, diverse.check())
	assert.Equal(t, actionWarn, repeating.check())
	failing := newLoopDetector()
	for i := range 5 {
		failing.record([]toolRecord{{name: "click", argsHash: uint64(i), resultHash: 12, failed: true}})
	}
	assert.Equal(t, actionBlock, failing.check())
}

func TestBrowserCheckpointDoesNotPersistProjectedFrames(t *testing.T) {
	ctx := context.Background()
	_, store, sessionID := newAttachmentsStore(t)
	ms := newMessageStore(store, sessionID, tool.BrowserAgentType)
	require.NoError(t, appendTestMessage(ctx, ms, &llmwire.Message{Role: llmwire.RoleUser, Content: "browse"}))
	for _, id := range []string{"one", "two"} {
		require.NoError(t, appendTestAssistant(ctx, ms, &llmwire.Response{ToolCalls: browserCall(id).ToolCalls}))
		frame := browserFrame(id, false)
		require.NoError(t, appendTestMessage(ctx, ms, &frame))
	}
	projected := ms.getMessages()
	require.Equal(t, supersededBrowserFrame, projected[2].Content)
	entries, err := compactionEntries(projected[1:], ms.rowIDs[1:])
	require.NoError(t, err)
	entries = append(
		[]sessionstore.CompactionEntry{{Message: &transcript.Message{Role: llmwire.RoleUser, Content: "summary"}}},
		entries...)
	_, err = store.Commit(ctx, sessionstore.Commit{
		SessionID: sessionID, RootID: sessionID, At: time.Now().UTC(),
		Replace: &sessionstore.Replace{HeadIDs: []int64{ms.rowIDs[0]}, Entries: entries},
	})
	require.NoError(t, err)
	rows, err := store.LoadActiveMessages(ctx, sessionID)
	require.NoError(t, err)
	for _, row := range rows {
		assert.NotContains(t, row.Content, supersededBrowserFrame)
	}
	assert.Equal(t, "frame-one", rows[2].Content)
	assert.NotEmpty(t, rows[2].Attachments)
	assert.Equal(t, "frame-two", rows[4].Content)
}

func TestBrowserFrameReplacementClearsMeasuredBaseline(t *testing.T) {
	ctx := context.Background()
	_, store, sessionID := newAttachmentsStore(t)
	ms := newMessageStore(store, sessionID, tool.BrowserAgentType)
	require.NoError(t, appendTestAssistant(ctx, ms, &llmwire.Response{ToolCalls: browserCall("one").ToolCalls}))
	first := browserFrame("one", false)
	require.NoError(t, appendTestMessage(ctx, ms, &first))
	s := newTestAgent()
	s.id, s.rootID, s.model = sessionID, sessionID, "model"
	s.store, s.ms = store, ms
	base := &sessionstore.ContextBaseline{Model: "model", PromptTokens: 90000, MessageCount: 2}
	_, err := store.Commit(
		ctx,
		sessionstore.Commit{SessionID: sessionID, State: sessionstore.StatePatch{ContextBaseline: base}},
	)
	require.NoError(t, err)
	_, kept := s.storeContextBaseline(base.PromptTokens, base.MessageCount, s.modelGeneration())
	require.True(t, kept)
	require.NoError(t, appendTestAssistant(ctx, ms, &llmwire.Response{ToolCalls: browserCall("two").ToolCalls}))
	failed := browserFrame("two", true)
	failed.Images = nil
	failedRow, err := storedMessage(&failed)
	require.NoError(t, err)
	failureCommit := s.newCommit()
	failureCommit.ToolResults = []*transcript.Message{failedRow}
	_, err = s.commit(ctx, failureCommit)
	require.NoError(t, err)
	assert.NotNil(t, s.loadContextBaseline())
	record, err := store.GetSession(ctx, sessionID)
	require.NoError(t, err)
	assert.NotNil(t, record.ContextBaseline())
	require.NoError(t, appendTestAssistant(ctx, ms, &llmwire.Response{ToolCalls: browserCall("three").ToolCalls}))
	third := browserFrame("three", false)
	thirdRow, err := storedMessage(&third)
	require.NoError(t, err)
	successCommit := s.newCommit()
	successCommit.ToolResults = []*transcript.Message{thirdRow}
	_, err = s.commit(ctx, successCommit)
	require.NoError(t, err)
	assert.Nil(t, s.loadContextBaseline())
	record, err = store.GetSession(ctx, sessionID)
	require.NoError(t, err)
	assert.Nil(t, record.ContextBaseline())
	size, estimated := s.projectContextSize()
	assert.True(t, estimated)
	assert.Less(t, size, base.PromptTokens)
	assert.Equal(t, supersededBrowserFrame, s.ms.getMessages()[1].Content)
}
