package session

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
)

var errStoreDown = errors.New("store is down")

// TestMessageStore_AppendFailureLeavesNothingInMemory asserts the durable-first
// order: a rejected insert must not leave a phantom message the agent can read.
func TestMessageStore_AppendFailureLeavesNothingInMemory(t *testing.T) {
	tests := []struct {
		name string
		add  func(ctx context.Context, ms *messageStore) error
	}{
		{
			name: "user",
			add: func(ctx context.Context, ms *messageStore) error {
				return ms.addUserMessage(ctx, "hello")
			},
		},
		{
			name: "assistant",
			add: func(ctx context.Context, ms *messageStore) error {
				return ms.addAssistantMessage(ctx, &llmwire.Response{Text: "hi"})
			},
		},
		{
			name: "tool_result",
			add: func(ctx context.Context, ms *messageStore) error {
				return ms.addToolResult(ctx, "c1", "read", "body")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			ms := newMessageStore(&mockSessionStore{insertErr: errStoreDown}, 1)

			err := tt.add(ctx, ms)
			require.Error(t, err)
			require.ErrorIs(t, err, errStoreDown)
			assert.Empty(t, ms.getMessages(), "failed write must not appear in the transcript")
		})
	}
}

func TestMessageStore_AppendStoresRowIDs(t *testing.T) {
	ctx := context.Background()
	ms := newMessageStore(&mockSessionStore{}, 1)

	require.NoError(t, ms.addUserMessage(ctx, "hello"))
	require.NoError(t, ms.addAssistantMessage(ctx, &llmwire.Response{Text: "hi"}))
	require.NoError(t, ms.addToolResult(ctx, "c1", "read", "body"))

	msgs := ms.getMessages()
	rowIDs := ms.getRowIDs()
	require.Len(t, msgs, 3)
	require.Len(t, rowIDs, 3)

	for i, rowID := range rowIDs {
		assert.Equal(t, int64(i+1), rowID, "message %d carries the id the store handed back", i)
	}
}

func TestAddToolNotificationPairOnce_InsertFailure(t *testing.T) {
	ctx := context.Background()
	ms := newMessageStore(&mockSessionStore{pairErr: errStoreDown}, 1)

	_, err := ms.addToolNotificationPairOnce(ctx, "d1", "c1", "sleep", "woke up")
	require.Error(t, err)
	require.ErrorIs(t, err, errStoreDown)
	assert.Empty(t, ms.getMessages())
}

// TestAddToolNotificationPairOnce_PersistsCallList guards the removed marshal
// fallback: the assistant stub must carry the tool_call LIST, never the raw args.
func TestAddToolNotificationPairOnce_PersistsCallList(t *testing.T) {
	ctx := context.Background()
	store := &mockSessionStore{}
	ms := newMessageStore(store, 1)

	applied, err := ms.addToolNotificationPairOnce(ctx, "d1", "c1", "sleep", "woke up")
	require.NoError(t, err)
	require.True(t, applied)

	want, err := json.Marshal(
		[]llmwire.ToolCall{{ID: "c1", Name: "sleep", Arguments: json.RawMessage("{}")}},
	)
	require.NoError(t, err)

	require.NotNil(t, store.lastPairAsst)
	assert.JSONEq(t, string(want), string(store.lastPairAsst.ToolCalls))

	rowIDs := ms.getRowIDs()
	require.Len(t, rowIDs, 2)
	assert.NotZero(t, rowIDs[0])
	assert.NotZero(t, rowIDs[1])
}

func TestBuildBackgroundSubagentCompletion_PersistsCallList(t *testing.T) {
	stored, err := BuildBackgroundSubagentCompletion(42, "child done")
	require.NoError(t, err)
	require.Len(t, stored, 2)

	var calls []llmwire.ToolCall
	require.NoError(t, json.Unmarshal(stored[0].ToolCalls, &calls))
	require.Len(t, calls, 1)
	assert.Equal(t, subagentEventTool, calls[0].Name)
	assert.JSONEq(t, `{"child_id":42,"event":"completed"}`, string(calls[0].Arguments))
}

func TestResetContextAndInjectOnce_MarkCompactedFailureKeepsTranscript(t *testing.T) {
	ctx := context.Background()
	store := &compactionRecordingStore{nextID: 1, markCompactedErr: errStoreDown}
	s := newResetTestSvc(store)

	seedResetTranscript(ctx, t, s)
	s.todoStore.Add("old todo", todo.PriorityMedium)

	_, err := s.ResetContextAndInjectOnce(ctx, "reset:fresh:1", "do the fresh job")
	require.Error(t, err)
	require.ErrorIs(t, err, errStoreDown)

	msgs := s.ms.getMessages()
	require.Len(t, msgs, 3)
	assert.Equal(t, "old task", msgs[0].Content)
	assert.Len(t, s.todoStore.List(), 1)
}

// TestResetContextAndInjectOnce_OpeningInsertFailureKeepsTranscript asserts the old
// rows are never hidden when the new opening turn could not be written.
func TestResetContextAndInjectOnce_OpeningInsertFailureKeepsTranscript(t *testing.T) {
	ctx := context.Background()
	store := &compactionRecordingStore{nextID: 1}
	s := newResetTestSvc(store)

	seedResetTranscript(ctx, t, s)

	store.insertErr = errStoreDown
	store.insertFailAt = store.insertCalls + 1

	_, err := s.ResetContextAndInjectOnce(ctx, "reset:fresh:1", "do the fresh job")
	require.Error(t, err)
	require.ErrorIs(t, err, errStoreDown)
	assert.Zero(t, store.markCompacted, "transcript must not be hidden when the opening failed")
	assert.Len(t, s.ms.getMessages(), 3)
}

func TestResetContextAndInjectOnce_NoStore(t *testing.T) {
	s := newResetTestSvc(nil)
	s.ms = newMessageStore(nil, 0)
	s.store = nil

	applied, err := s.ResetContextAndInjectOnce(context.Background(), "reset:fresh:1", "do the fresh job")
	require.NoError(t, err)
	require.True(t, applied)

	msgs := s.ms.getMessages()
	require.Len(t, msgs, 2)
	assert.Contains(t, msgs[1].Content, "do the fresh job")
}

func seedResetTranscript(ctx context.Context, t *testing.T, s *svc) {
	t.Helper()

	for _, m := range []llmwire.Message{
		{Role: llmwire.RoleUser, Content: "old task"},
		{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "c1", Name: "read"}}},
		{Role: llmwire.RoleTool, Content: "old result", ToolCallID: "c1", ToolName: "read"},
	} {
		s.ms.mu.Lock()
		err := s.ms.appendMessageLocked(ctx, &m)
		s.ms.mu.Unlock()
		require.NoError(t, err)
	}
}

// TestRun_OpeningWriteFailure_SkipsCheckpoint covers the pre-loop abort: no
// iteration happened, so Run must not try to checkpoint a store it just saw fail.
func TestRun_OpeningWriteFailure_SkipsCheckpoint(t *testing.T) {
	store := &mockSessionStore{insertErr: errStoreDown}
	s := newMockSvc(t, nil, "")
	s.store = store
	s.ms = newMessageStore(store, 1)

	_, err := s.Run(context.Background(), "write tests")
	require.Error(t, err)
	require.ErrorIs(t, err, errStoreDown)
	assert.Zero(t, store.iterationCalls, "no checkpoint attempted before the loop")
}

// TestRun_LoopWriteFailure_KeepsOriginalError asserts the join in the error path
// does not swallow the write failure that actually stopped the run.
func TestRun_LoopWriteFailure_KeepsOriginalError(t *testing.T) {
	store := &mockSessionStore{insertErr: errStoreDown, insertFailAt: 2, failCall: 2}
	s := newMockSvc(t, nil, "")
	s.llmClient = &mockLLMRunOnce{response: &llmwire.Response{Text: "done"}}
	s.store = store
	s.ms = newMessageStore(store, 1)

	_, err := s.Run(context.Background(), "write tests")
	require.Error(t, err)
	require.ErrorIs(t, err, errStoreDown, "the write failure survives the checkpoint error")
	assert.Contains(t, err.Error(), "persist checkpoint error state")
}

func TestAddToolNotificationPairOnce_NoStore(t *testing.T) {
	ms := newMessageStore(nil, 0)

	_, err := ms.addToolNotificationPairOnce(context.Background(), "d1", "c1", "sleep", "woke up")
	require.Error(t, err, "an idempotent notification without a durable store must fail closed")
	assert.Empty(t, ms.getMessages())
}

// TestResetContextAndInjectOnce_SecondOpeningInsertFailure covers the multi-message
// opening turn: the header lands, the prompt does not, and the old rows still
// must not be hidden.
func TestResetContextAndInjectOnce_SecondOpeningInsertFailure(t *testing.T) {
	ctx := context.Background()
	store := &compactionRecordingStore{nextID: 1}
	s := newResetTestSvc(store)

	seedResetTranscript(ctx, t, s)
	require.NotEmpty(t, s.agentsMD, "the opening turn must be two messages for this case")

	store.insertErr = errStoreDown
	store.insertFailAt = store.insertCalls + 2

	_, err := s.ResetContextAndInjectOnce(ctx, "reset:fresh:1", "do the fresh job")
	require.Error(t, err)
	require.ErrorIs(t, err, errStoreDown)
	assert.Zero(t, store.markCompacted, "transcript must not be hidden when the opening is incomplete")
	assert.Len(t, s.ms.getMessages(), 3, "in-memory transcript is untouched")
}

// TestExecuteToolCalls_WriteFailurePropagates covers both write loops — the
// loop-detector block path and the normal result path.
func TestExecuteToolCalls_WriteFailurePropagates(t *testing.T) {
	tests := []struct {
		name    string
		blocked bool
	}{
		{name: "normal path"},
		{name: "loop detector blocked", blocked: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := newTestAgent(&stubTool{id: "read", result: "content"})
			agent.ms = newMessageStore(&mockSessionStore{insertErr: errStoreDown}, 1)
			agent.loopDetector.blocked = tt.blocked

			tc := llmwire.ToolCall{ID: "tc_1", Name: "read", Arguments: []byte(`{}`)}
			err := executeToolCalls(context.Background(), agent, []llmwire.ToolCall{tc})

			require.ErrorIs(t, err, errStoreDown)
			assert.Empty(t, agent.ms.getMessages())
		})
	}
}

// TestHandlePreviousResult_WriteErrorBeatsSuspend: a suspend request must not
// mask a tool result that never reached the store — that would report a session
// state the transcript does not back.
func TestHandlePreviousResult_WriteErrorBeatsSuspend(t *testing.T) {
	agent := newTestAgent(
		&stubTool{id: "sleep", err: tool.ErrSuspend},
		&stubTool{id: "read", result: "content"},
	)
	agent.ms = newMessageStore(&mockSessionStore{insertErr: errStoreDown}, 1)
	agent.ms.setMessages([]llmwire.Message{
		{Role: llmwire.RoleUser, Content: "task"},
		{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{
			{ID: "tc_sleep", Name: "sleep", Arguments: []byte(`{}`)},
			{ID: "tc_read", Name: "read", Arguments: []byte(`{}`)},
		}},
	})

	r := &loopRunner{agent: agent, result: &loopResult{}, log: zap.NewNop()}

	done, err := r.handlePreviousResult(context.Background())
	require.ErrorIs(t, err, errStoreDown)
	require.True(t, agent.suspended, "the suspend request was made — the error must still win")
	assert.False(t, done, "a failed write does not end the loop as a clean suspend")
	assert.False(t, r.result.Suspended)
}
