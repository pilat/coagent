package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/transcript"
)

func TestHarnessScenario_AssistantBackgroundResult(t *testing.T) {
	processRunning := make(chan struct{})
	t.Cleanup(func() { closeOnce(processRunning) })
	h := newHarness(t, harnessOptions{respond: func(system string, messages []llmwire.Message) *llmwire.Response {
		if strings.HasPrefix(system, registry.AssistantPrompt) {
			return textReply("full result")
		}

		if hasUserContaining(messages, "<process_completion>") {
			return textReply("answer after process")
		}

		<-processRunning

		return textReply("yield before process")
	}})
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	work, err := h.store.CreateSession(h.ctx, h.projectID, "fake-model", "", managerAttrs(scenarioManagerID))
	require.NoError(t, err)
	service := installScenarioProcessService(t, h)
	a := assistantFor(h, root.ID)
	require.NoError(t, a.Send(h.ctx, work.ID, "work with process"))
	release := filepath.Join(t.TempDir(), "release")
	startScenarioProcess(
		t,
		service,
		work.ID,
		work.ID,
		fmt.Sprintf("while [ ! -f '%s' ]; do sleep 0.05; done; echo done", release),
	)
	close(processRunning)
	h.waitUntil("work yielded", func() bool { return !h.mgr.HasActiveLoop(work.ID) })
	assert.Zero(t, assistantEventCount(t, h, root.ID))
	assert.True(t, assistantPending(t, h, root.ID, work.ID))
	require.NoError(t, os.WriteFile(release, nil, 0o600))
	h.waitUntil("process result retold", func() bool { return assistantSayCount(t, h, root.ID, "full result") == 1 })
	var body string
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT raw_content FROM session_inbox
		WHERE session_id = ? AND source = 'agent'`, root.ID).Scan(&body))
	assert.Contains(t, body, `yield before process\n\n---\n\nanswer after process`,
		"every final is shown, oldest first")
	assert.Equal(t, 1, strings.Count(body, "answer after process"), "a repeated final is shown once")
}

func TestHarnessScenario_AssistantBootObservesCompletedSend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.db")
	first := newHarness(t, harnessOptions{dbPath: path})
	root, err := first.mgr.EnsureAssistantRoot(first.ctx, scenarioManagerID)
	require.NoError(t, err)
	work, err := first.store.CreateSession(
		first.ctx,
		first.projectID,
		"fake-model",
		"",
		managerAttrs(scenarioManagerID),
	)
	require.NoError(t, err)
	in, err := first.store.Enqueue(first.ctx, sessionstore.Input{
		SessionID: work.ID, Source: sessionstore.InputSourceUser, Content: "finish before crash",
		Attributes: map[string]any{"from": "assistant", sessionstore.ObserverSessionIDAttribute: root.ID},
	})
	require.NoError(t, err)
	_, err = first.store.Commit(first.ctx, sessionstore.Commit{SessionID: work.ID, Accept: []sessionstore.Accept{
		{InputID: in.Input.ID, State: sessionstore.InputStateAccepted, Content: "finish before crash", LinkRef: -1},
	}})
	require.NoError(t, err)

	completed := sessionstore.SessionStatusCompleted
	_, err = first.store.Commit(first.ctx, sessionstore.Commit{
		SessionID: work.ID,

		Messages: []*transcript.Message{{Role: "assistant", Content: "finished before crash", FinishType: "stop"}},
		State:    sessionstore.StatePatch{Status: &completed},
		Outputs: []sessionstore.Output{
			{
				Type:          sessionstore.OutputMessagePersistent,
				Content:       "finished before crash",
				Phase:         "final",
				MessageRef:    0,
				ReleasesInput: true,
			},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, assistantEventCount(t, first, root.ID))
	assert.False(t, first.mgr.HasActiveLoop(root.ID))
	first.shutdown()
	second := newHarness(
		t,
		harnessOptions{dbPath: path, respond: func(_ string, _ []llmwire.Message) *llmwire.Response {
			return textReply("recovered answer")
		}},
	)
	require.NoError(t, second.mgr.Start(second.ctx))
	second.waitUntil(
		"recovered observation",
		func() bool { return assistantSayCount(t, second, root.ID, "recovered answer") == 1 },
	)
	assert.Equal(t, 1, assistantEventCount(t, second, root.ID))
}

func TestHarnessScenario_AssistantShutdownAfterCommitKeepsEffects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tool-step.db")
	opened, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { closeOnce(release) })
	first := newHarness(
		t,
		harnessOptions{dbPath: path, respond: func(system string, messages []llmwire.Message) *llmwire.Response {
			if strings.HasPrefix(system, registry.AssistantPrompt) && !hasToolResultFor(messages, "open") {
				return assistantCalls(
					"",
					llmwire.ToolCall{
						ID:   "open-1",
						Name: "open",
						Arguments: []byte(
							`{"project":"durable-work","about":"persistent opened work","text":"finish the work"}`,
						),
					},
				)
			}
			if strings.HasPrefix(system, registry.AssistantPrompt) {
				closeOnce(opened)
			}
			<-release
			return textReply("reply after open")
		}},
	)
	root, err := first.mgr.EnsureAssistantRoot(first.ctx, scenarioManagerID)
	require.NoError(t, err)
	require.NoError(t, first.mgr.sendToSession(first.ctx, root.ID, "open work"))
	requireSignal(t, opened)
	var workID int64
	require.NoError(t, first.db.QueryRowContext(first.ctx, `SELECT session_id FROM assistant_work`).Scan(&workID))
	assert.Equal(t, 0, assistantSayCount(t, first, root.ID, "reply after open"))
	first.shutdown()
	close(release)
	seenDesk := make(chan struct{})
	second := newHarness(
		t,
		harnessOptions{dbPath: path, respond: func(system string, messages []llmwire.Message) *llmwire.Response {
			if strings.HasPrefix(system, registry.AssistantPrompt) {
				if len(messages) > 0 && strings.Contains(messages[0].Content, "persistent opened work") {
					closeOnce(seenDesk)
				}
				return textReply("recovered answer")
			}
			return textReply("recovered work")
		}},
	)
	require.NoError(t, second.mgr.Start(second.ctx))
	second.waitUntil("resumed work and assistant settle", func() bool {
		return assistantSayCount(t, second, root.ID, "recovered answer") > 0 &&
			!second.mgr.HasActiveLoop(root.ID) && !second.mgr.HasActiveLoop(workID)
	})
	require.NoError(t, second.mgr.sendToSession(second.ctx, root.ID, "show opened work"))
	requireSignal(t, seenDesk)
	second.waitUntil("new activation settled", func() bool { return !second.mgr.HasActiveLoop(root.ID) })
	var workCount, toolResults int
	require.NoError(t, second.db.QueryRowContext(second.ctx, `SELECT COUNT(*) FROM assistant_work`).Scan(&workCount))
	require.NoError(
		t,
		second.db.QueryRowContext(second.ctx, `SELECT COUNT(*) FROM messages WHERE session_id = ? AND role = 'tool' AND tool_name = 'open'`, root.ID).
			Scan(&toolResults),
	)
	assert.Equal(t, 1, workCount, "a committed open is not executed again after restart")
	assert.Equal(t, 1, toolResults)
	work, err := second.store.GetSession(second.ctx, workID)
	require.NoError(t, err)
	assert.Equal(t, "fake-model", work.Model)
}

func TestHarnessScenario_AssistantWaitsForWorkSleep(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	work, err := h.store.CreateSession(h.ctx, h.projectID, "fake-model", "", managerAttrs(scenarioManagerID))
	require.NoError(t, err)
	in, err := h.store.Enqueue(h.ctx, sessionstore.Input{
		SessionID: work.ID, Source: sessionstore.InputSourceUser, Content: "merge when green",
		Attributes: map[string]any{"from": "assistant", sessionstore.ObserverSessionIDAttribute: root.ID},
	})
	require.NoError(t, err)

	_, err = h.store.Commit(h.ctx, sessionstore.Commit{
		SessionID: work.ID,
		Accept: []sessionstore.Accept{
			{InputID: in.Input.ID, State: sessionstore.InputStateAccepted, Content: "merge when green", LinkRef: -1},
		},
		Messages: []*transcript.Message{
			{Role: "assistant", Content: "merging, waiting for CI", FinishType: "stop"},
			{Role: "assistant", ToolCalls: json.RawMessage(`[{"ID":"call_sleep","Name":"sleep","Arguments":{}}]`)},
		},
	})
	require.NoError(t, err)

	assert.Zero(t, assistantEventCount(t, h, root.ID), "a session paused inside a tool call has not answered")

	_, err = h.store.Commit(h.ctx, sessionstore.Commit{SessionID: work.ID, ToolResults: []*transcript.Message{
		{Role: "tool", ToolCallID: "call_sleep", ToolName: "sleep", Content: "ready"},
	}})
	require.NoError(t, err)

	_, err = h.store.Commit(h.ctx, sessionstore.Commit{
		SessionID: work.ID,
		Messages:  []*transcript.Message{{Role: "assistant", Content: "both merged", FinishType: "stop"}},
		State:     sessionstore.StatePatch{Status: new(sessionstore.SessionStatusCompleted)},
		Outputs: []sessionstore.Output{
			{
				Type:          sessionstore.OutputMessagePersistent,
				Content:       "both merged",
				Phase:         "final",
				MessageRef:    0,
				ReleasesInput: true,
			},
		},
	})
	require.NoError(t, err)
	var body string
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT raw_content FROM session_inbox
		WHERE session_id = ? AND source = 'agent'`, root.ID).Scan(&body))
	assert.Contains(t, body, "merging, waiting for CI")
	assert.Contains(t, body, "both merged")
}
