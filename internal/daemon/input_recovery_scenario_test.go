package daemon

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/admission"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/transcript"
)

func TestHarnessScenario_RestartResumesExplicitInputQueuedOnStoppedRoot(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "stopped-explicit-resume.db")
	var modelCalls atomic.Int64
	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		modelCalls.Add(1)
		require.True(t, hasUserContaining(messages, "retained process fact"))
		require.True(t, hasUserContaining(messages, "explicit resume after stop"))

		return &llmwire.Response{Text: "stopped root resumed after restart"}
	}

	first := newSubagentHarnessOnDB(t, dbPath, respond, nil)
	root, err := first.sessStore.CreateSession(first.ctx, first.projectID, "fake-model", "", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)
	require.NoError(t, first.sessStore.UpdateSessionStatus(
		first.ctx, root.ID, sessionstore.SessionStatusStopped,
	))
	_, err = first.sessStore.EnqueueAsyncInput(
		first.ctx, root.ID, sessionstore.InputSourceProcess, "retained process fact", nil,
	)
	require.NoError(t, err)
	_, err = first.sessStore.EnqueueInput(
		first.ctx, root.ID, sessionstore.InputSourceUser, "explicit resume after stop",
	)
	require.NoError(t, err)
	first.shutdown()

	second := newSubagentHarnessOnDB(t, dbPath, respond, nil)
	collector := collectEvents(second.mgr.PubSub().SubscribeAll())
	defer func() {
		collector.stop()
		second.shutdown()
	}()
	second.mgr.sweep(second.ctx)
	waitForVisibleMessage(t, collector, root.ID, "stopped root resumed after restart")
	drainScenarioClaims(t, "explicit_stopped_resume_restart.json", newChainController(t, second))
	waitForIdleAfterMessage(t, collector, root.ID, "stopped root resumed after restart")
	assert.Equal(t, int64(1), modelCalls.Load())
	assertHarnessTrace(t, "explicit_stopped_resume_restart.json", collector.snapshot(), root.ID)
}

func TestHarnessScenario_RestartConsumesReadOnlyInputQueuedOnStoppedRoot(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "stopped-read-only.db")
	var modelCalls atomic.Int64
	respond := func(string, []llmwire.Message) *llmwire.Response {
		modelCalls.Add(1)

		return &llmwire.Response{Text: "must not call model"}
	}

	first := newSubagentHarnessOnDB(t, dbPath, respond, nil)
	root, err := first.sessStore.CreateSession(first.ctx, first.projectID, "fake-model", "", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)
	require.NoError(t, first.sessStore.UpdateSessionStatus(
		first.ctx, root.ID, sessionstore.SessionStatusStopped,
	))
	_, err = first.sessStore.EnqueueInput(first.ctx, root.ID, sessionstore.InputSourceUser, "/help")
	require.NoError(t, err)
	asyncInput, err := first.sessStore.EnqueueAsyncInput(
		first.ctx, root.ID, sessionstore.InputSourceProcess, "retained process fact", nil,
	)
	require.NoError(t, err)
	first.shutdown()

	second := newSubagentHarnessOnDB(t, dbPath, respond, nil)
	beforeRecovery, err := second.sessStore.GetSession(second.ctx, root.ID)
	require.NoError(t, err)
	preserveStopped, err := second.mgr.commandOnlyStoppedRoot(second.ctx, beforeRecovery)
	require.NoError(t, err)
	require.True(t, preserveStopped)
	collector := collectEvents(second.mgr.PubSub().SubscribeAll())
	defer func() {
		collector.stop()
		second.shutdown()
	}()
	second.mgr.sweep(second.ctx)
	collector.waitFor(t, "session help", func(events []controllerapi.SessionNotification) bool {
		return slices.ContainsFunc(events, func(event controllerapi.SessionNotification) bool {
			return event.SessionID == root.ID && event.Notification.Type == sessionevent.NotifyMessage &&
				strings.Contains(event.Notification.Message, "## Session commands")
		})
	})
	drainScenarioClaims(t, "stopped_read_only_restart.json", newChainController(t, second))
	second.waitUntil("read-only recovery preserved stop", func() bool {
		record, getErr := second.sessStore.GetSession(second.ctx, root.ID)

		return getErr == nil && record.Status == sessionstore.SessionStatusStopped &&
			!second.mgr.HasActiveLoop(root.ID)
	})

	record, err := second.sessStore.GetSession(second.ctx, root.ID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.SessionStatusStopped, record.Status)
	assert.Zero(t, modelCalls.Load())
	pending, err := second.sessStore.PeekPending(second.ctx, root.ID)
	require.NoError(t, err)
	assert.Equal(t, asyncInput.ID, pending.ID)
	assert.Equal(t, sessionstore.InputSourceProcess, pending.Source)
	assertHarnessTrace(t, "stopped_read_only_restart.json", collector.snapshot(), root.ID)
}

func TestScenario_RestartResumesExplicitInputQueuedOnErroredChild(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "errored-child-explicit-resume.db")
	first := newSubagentHarnessOnDB(t, dbPath, func(string, []llmwire.Message) *llmwire.Response {
		return &llmwire.Response{Text: "must remain queued"}
	}, nil)
	root, err := first.sessStore.CreateSession(first.ctx, first.projectID, "fake-model", "", nil)
	require.NoError(t, err)
	childID, err := first.mgr.subagents.Create(first.ctx, subagent.Create{
		ProjectID: first.projectID, ParentID: root.ID, RootID: root.ID,
		Model: "fake-model", TaskCallID: "errored-child", State: subagent.StateRunning,
	})
	require.NoError(t, err)
	require.NoError(t, first.links.MarkLinkTerminal(
		first.ctx, childID, subagent.StateError, "old failure", subagent.OutcomeError,
	))
	require.NoError(t, first.sessStore.UpdateSessionStatus(
		first.ctx, childID, sessionstore.SessionStatusError,
	))
	link, err := first.links.GetLink(first.ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, link)
	won, err := first.mgr.subagents.DeliverBackgroundCompletion(first.ctx, *link, 1)
	require.NoError(t, err)
	require.True(t, won)
	_, err = first.sessStore.EnqueueInput(
		first.ctx, childID, sessionstore.InputSourceAgent, "explicit retry after error",
	)
	require.NoError(t, err)
	require.NoError(t, first.sessStore.UpdateSessionStatus(
		first.ctx, root.ID, sessionstore.SessionStatusStopped,
	))
	first.shutdown()

	second := newSubagentHarnessOnDB(t, dbPath, func(string, []llmwire.Message) *llmwire.Response {
		return &llmwire.Response{Text: "must remain queued"}
	}, nil)
	defer second.shutdown()
	for i := range admission.MaxChildren {
		require.True(t, second.mgr.admit.TryAdmit(admission.Child, int64(30_000+i)))
		defer second.mgr.admit.Release(admission.Child, int64(30_000+i))
	}

	resumed, err := second.mgr.resumeSessionsWithRecoverableInput(second.ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, resumed)
	link, err = second.links.GetLink(second.ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.Equal(t, subagent.StateRunning, link.State)
	assert.Equal(t, int64(2), link.ActivationSeq)
	pending, err := second.sessStore.PeekPending(second.ctx, childID)
	require.NoError(t, err)
	assert.Equal(t, "explicit retry after error", pending.RawContent)
}

func TestHarnessScenario_RestartResumesAcceptedInputWithoutAssistant(t *testing.T) {
	runAcceptedInputRestartScenario(t, nil, "accepted_input_restart_recovery.json")
}

func TestHarnessScenario_RestartResumesAcceptedInputAfterToolResult(t *testing.T) {
	runAcceptedInputRestartScenario(
		t, appendCrashToolProgress, "accepted_input_tool_progress_restart.json",
	)
}

func TestHarnessScenario_RestartSettlesPersistedFinalWithoutRepublishing(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "persisted-final.db")
	var modelCalls atomic.Int64
	respond := func(_ string, _ []llmwire.Message) *llmwire.Response {
		modelCalls.Add(1)
		return &llmwire.Response{Text: "must not run"}
	}

	first := newSubagentHarnessOnDB(t, dbPath, respond, nil)
	root, err := first.sessStore.CreateSession(first.ctx, first.projectID, "fake-model", "", nil)
	require.NoError(t, err)
	input, err := first.sessStore.EnqueueInput(
		first.ctx, root.ID, sessionstore.InputSourceUser, "answered before crash",
	)
	require.NoError(t, err)
	_, err = first.sessStore.PromoteInput(first.ctx, input.ID, "[user] answered before crash")
	require.NoError(t, err)
	_, err = first.sessStore.InsertMessage(first.ctx, root.ID, &transcript.Message{
		Role: llmwire.RoleAssistant, Content: "persisted final",
	})
	require.NoError(t, err)
	first.shutdown()

	second := newSubagentHarnessOnDB(t, dbPath, respond, nil)
	collector := collectEvents(second.mgr.PubSub().SubscribeAll())
	defer func() {
		collector.stop()
		second.shutdown()
	}()

	second.mgr.sweep(second.ctx)
	collector.waitFor(t, "persisted final settled", func(events []controllerapi.SessionNotification) bool {
		return slices.ContainsFunc(events, func(event controllerapi.SessionNotification) bool {
			return event.SessionID == root.ID &&
				event.Notification.Type == sessionevent.NotifyStateChanged &&
				event.Notification.Status == sessionevent.StateIdle
		})
	})

	assert.Zero(t, modelCalls.Load(), "a durable final answer must not call the model again")
	assert.Zero(t, countPublishedMessage(collector.snapshot(), root.ID, "persisted final"),
		"historical output is state, not a new publication")
	record, err := second.sessStore.GetSession(second.ctx, root.ID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.SessionStatusCompleted, record.Status)
	assertHarnessTrace(t, "accepted_input_persisted_final_restart.json", collector.snapshot(), root.ID)
}

func TestHarnessScenario_RestartDoesNotRunHandledHeaderOnlySession(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "handled-header.db")
	var modelCalls atomic.Int64
	respond := func(_ string, _ []llmwire.Message) *llmwire.Response {
		modelCalls.Add(1)
		return &llmwire.Response{Text: "must not run"}
	}

	first := newSubagentHarnessOnDB(t, dbPath, respond, nil)
	root, err := first.sessStore.CreateSession(first.ctx, first.projectID, "fake-model", "", nil)
	require.NoError(t, err)
	_, err = first.sessStore.InsertMessage(first.ctx, root.ID, &transcript.Message{
		Role: llmwire.RoleUser, Content: "User preferences from AGENTS.md files:\n\nheader only",
	})
	require.NoError(t, err)
	input, err := first.sessStore.EnqueueInput(first.ctx, root.ID, sessionstore.InputSourceUser, "/status")
	require.NoError(t, err)
	require.NoError(t, first.sessStore.HandleInput(first.ctx, input.ID, "status command"))
	first.shutdown()

	second := newSubagentHarnessOnDB(t, dbPath, respond, nil)
	collector := collectEvents(second.mgr.PubSub().SubscribeAll())
	defer func() {
		collector.stop()
		second.shutdown()
	}()

	second.mgr.sweep(second.ctx)
	assert.False(t, second.mgr.HasActiveLoop(root.ID))
	assert.Zero(t, modelCalls.Load())
	assert.Empty(t, collector.snapshot(), "handled control input must not create recovery events")
}

func runAcceptedInputRestartScenario(
	t *testing.T,
	afterInput func(*testing.T, *subagentHarness, int64),
	traceName string,
) {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "accepted-input.db")
	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasUserContaining(messages, "accepted before crash") {
			return &llmwire.Response{Text: "accepted input recovered"}
		}

		return &llmwire.Response{Text: "unexpected prompt"}
	}

	first := newSubagentHarnessOnDB(t, dbPath, respond, nil)
	root, err := first.sessStore.CreateSession(first.ctx, first.projectID, "fake-model", "", nil)
	require.NoError(t, err)
	input, err := first.sessStore.EnqueueInput(
		first.ctx, root.ID, sessionstore.InputSourceUser, "accepted before crash",
	)
	require.NoError(t, err)
	_, err = first.sessStore.PromoteInput(first.ctx, input.ID, "[user] accepted before crash")
	require.NoError(t, err)
	if afterInput != nil {
		afterInput(t, first, root.ID)
	}
	first.shutdown()

	second := newSubagentHarnessOnDB(t, dbPath, respond, nil)
	collector := collectEvents(second.mgr.PubSub().SubscribeAll())
	defer func() {
		collector.stop()
		second.shutdown()
	}()

	second.mgr.sweep(second.ctx)
	waitForVisibleMessage(t, collector, root.ID, "accepted input recovered")
	waitForIdleAfterMessage(t, collector, root.ID, "accepted input recovered")

	assert.Equal(t, "accepted input recovered", lastAssistantTextDTO(second.parentMessages(root.ID)))
	require.NoError(t, llm.ValidateToolPairing(second.parentMessages(root.ID)))
	assertHarnessTrace(t, traceName, collector.snapshot(), root.ID)
}

func appendCrashToolProgress(t *testing.T, h *subagentHarness, sessionID int64) {
	t.Helper()

	calls, err := json.Marshal([]llmwire.ToolCall{{
		ID: "crash-tool", Name: "read", Arguments: []byte(`{"path":"README.md"}`),
	}})
	require.NoError(t, err)
	_, err = h.sessStore.InsertMessage(h.ctx, sessionID, &transcript.Message{
		Role: llmwire.RoleAssistant, ToolCalls: calls,
	})
	require.NoError(t, err)
	_, err = h.sessStore.InsertMessage(h.ctx, sessionID, &transcript.Message{
		Role: llmwire.RoleTool, ToolCallID: "crash-tool", ToolName: "read", Content: "durable tool result",
	})
	require.NoError(t, err)
}
