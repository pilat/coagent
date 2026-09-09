package daemon

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/admission"
	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

func TestHarnessScenario_ProcessCompletionAtBusyToolBoundary(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var modelCalls atomic.Int64
	var observed []llmwire.Message
	var observedMu sync.Mutex

	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		modelCalls.Add(1)
		if hasUserContaining(messages, "<process_completion>") {
			observedMu.Lock()
			observed = slices.Clone(messages)
			observedMu.Unlock()

			return &llmwire.Response{Text: "busy process completion observed"}
		}
		if hasUserContaining(messages, "start busy process scenario") {
			once.Do(func() { close(entered) })
			<-release

			return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
				ID: "busy-read", Name: "ls", Arguments: []byte(`{"path":"."}`),
			}}}
		}

		return &llmwire.Response{Text: "unexpected activation"}
	}

	h := newSubagentHarnessWith(t, respond)
	service := installScenarioProcessService(t, h)
	collector := collectEvents(h.mgr.PubSub().SubscribeAll())
	defer func() {
		closeOnce(release)
		collector.stop()
		h.shutdown()
	}()

	rootID, err := h.mgr.Send(h.ctx, h.projectID, "start busy process scenario", "fake-model", nil)
	require.NoError(t, err)
	<-entered
	process := startScenarioProcess(t, service, rootID, rootID, "printf 'finished\\n'")
	waitScenarioProcessState(t, h, process.ID, backgroundprocess.StateCompleted)
	close(release)
	waitForVisibleMessage(t, collector, rootID, "busy process completion observed")

	observedMu.Lock()
	messages := slices.Clone(observed)
	observedMu.Unlock()
	toolResult := slices.IndexFunc(messages, func(message llmwire.Message) bool {
		return message.Role == llmwire.RoleTool && message.ToolCallID == "busy-read"
	})
	completion := slices.IndexFunc(messages, func(message llmwire.Message) bool {
		return message.Role == llmwire.RoleUser && strings.Contains(message.Content, "<process_completion>")
	})
	assert.NotEqual(t, -1, toolResult)
	assert.Greater(t, completion, toolResult, "completion must not split the assistant/tool-result batch")
	assert.Equal(t, 1, countUserCompletions(messages, "<process_completion>"))

	var state string
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT state FROM session_inbox
		WHERE source = 'process' AND json_extract(attributes, '$.process_id') = ?`, process.ID).Scan(&state))
	assert.Equal(t, string(sessionstore.InputStateAccepted), state)
	drainScenarioClaims(t, "process_busy_boundary.json", newChainController(t, h))
	waitForIdleAfterMessage(t, collector, rootID, "busy process completion observed")
	assert.Equal(t, int64(2), modelCalls.Load())
	assertHarnessTrace(t, "process_busy_boundary.json", collector.snapshot(), rootID)
}

func TestHarnessScenario_ProcessCompletionAtIdleTransition(t *testing.T) {
	var modelCalls atomic.Int64
	h := newSubagentHarnessWith(t, func(_ string, messages []llmwire.Message) *llmwire.Response {
		modelCalls.Add(1)
		require.True(t, hasUserContaining(messages, "<process_completion>"))

		return &llmwire.Response{Text: "idle-transition completion observed"}
	})
	collector := collectEvents(h.mgr.PubSub().SubscribeAll())
	defer func() {
		collector.stop()
		h.shutdown()
	}()

	root, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)
	require.NoError(t, h.sessStore.UpdateSessionStatus(
		h.ctx, root.ID, sessionstore.SessionStatusCompleted,
	))
	_, err = h.mgr.inboxStore.EnqueueAsyncInput(
		h.ctx, root.ID, sessionstore.InputSourceProcess,
		"<process_completion>idle transition</process_completion>",
		map[string]any{"process_id": "idle-transition"},
	)
	require.NoError(t, err)

	workDir, err := h.mgr.store.GetProjectWorkDir(h.ctx, h.projectID)
	require.NoError(t, err)
	require.True(t, h.mgr.admit.TryAdmit(admission.Parent, 0))
	ending := newRunner(func() {}, workDir, h.projectID, admission.Parent, 0, false, nil)
	_, registered := h.mgr.runners.Register(root.ID, ending)
	require.True(t, registered)

	unlock, err := h.mgr.lockSessionTree(h.ctx, root.ID)
	require.NoError(t, err)
	require.NoError(t, h.mgr.inputReady(h.ctx, root.ID))
	leftover, deliver, continued := h.mgr.finishRunnerLocked(
		h.ctx, nil, root.ID, ending, false, false, true,
	)
	unlock()
	assert.Empty(t, leftover)
	assert.Nil(t, deliver)
	assert.True(t, continued, "teardown must reroute the wake into a replacement runner")

	waitForVisibleMessage(t, collector, root.ID, "idle-transition completion observed")
	drainScenarioClaims(t, "process_idle_transition.json", newChainController(t, h))
	waitForIdleAfterMessage(t, collector, root.ID, "idle-transition completion observed")
	assert.Equal(t, int64(1), modelCalls.Load())
	assertHarnessTrace(t, "process_idle_transition.json", collector.snapshot(), root.ID)
}

func TestHarnessScenario_ProcessCompletionRevivesCompletedRoot(t *testing.T) {
	var calls atomic.Int64
	h := newSubagentHarnessWith(t, func(_ string, messages []llmwire.Message) *llmwire.Response {
		calls.Add(1)
		require.True(t, hasUserContaining(messages, "<process_completion>"))

		return &llmwire.Response{Text: "idle process completion observed"}
	})
	service := installScenarioProcessService(t, h)
	collector := collectEvents(h.mgr.PubSub().SubscribeAll())
	defer func() {
		collector.stop()
		h.shutdown()
	}()

	root, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)
	require.NoError(t, h.sessStore.UpdateSessionStatus(
		h.ctx, root.ID, sessionstore.SessionStatusCompleted,
	))

	process := startScenarioProcess(t, service, root.ID, root.ID, "printf 'finished\\n'")
	waitForVisibleMessage(t, collector, root.ID, "idle process completion observed")
	drainScenarioClaims(t, "process_completed_root.json", newChainController(t, h))
	waitForIdleAfterMessage(t, collector, root.ID, "idle process completion observed")

	assert.Equal(t, int64(1), calls.Load())
	assert.Equal(t, 1, countUserCompletions(h.parentMessages(root.ID), "<process_completion>"))
	var persistent, replaceable, releasing int
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT
		COUNT(*) FILTER (WHERE type = 'message_persistent' AND content = 'idle process completion observed'),
		COUNT(*) FILTER (WHERE type = 'message_replaceable' AND content = 'idle process completion observed'),
		COUNT(*) FILTER (WHERE releases_input = 1 AND content = 'idle process completion observed')
		FROM session_outbox WHERE session_id = ?`, root.ID).Scan(&persistent, &replaceable, &releasing))
	assert.Zero(t, persistent, "process-only input must not create a manager direct reply")
	assert.Equal(t, 1, replaceable)
	assert.Equal(t, 1, releasing, "terminal progress still releases the accepted asynchronous input")
	var state string
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT state FROM session_inbox
		WHERE source = 'process' AND json_extract(attributes, '$.process_id') = ?`, process.ID).Scan(&state))
	assert.Equal(t, string(sessionstore.InputStateAccepted), state)
	assertHarnessTrace(t, "process_completed_root.json", collector.snapshot(), root.ID)
}

func TestHarnessScenario_ProcessCompletionInterruptsSleep(t *testing.T) {
	var modelCalls atomic.Int64
	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		modelCalls.Add(1)
		if hasUserContaining(messages, "<process_completion>") {
			return &llmwire.Response{Text: "process interrupted sleep"}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID: "process-sleep", Name: tool.IDSleep,
			Arguments: []byte(`{"duration":"1h","reason":"wait for process"}`),
		}}}
	}

	h := newSubagentHarnessWith(t, respond)
	service := installScenarioProcessService(t, h)
	collector := collectEvents(h.mgr.PubSub().SubscribeAll())
	defer func() {
		collector.stop()
		h.shutdown()
	}()

	rootID, err := h.mgr.Send(h.ctx, h.projectID, "start process sleep", "fake-model", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)
	waitForWaitKind(t, collector, rootID, sessionevent.WaitSleep)
	process := startScenarioProcess(t, service, rootID, rootID, "printf 'wake\\n'")
	waitForVisibleMessage(t, collector, rootID, "process interrupted sleep")
	drainScenarioClaims(t, "process_interrupts_sleep.json", newChainController(t, h))
	waitForIdleAfterMessage(t, collector, rootID, "process interrupted sleep")

	messages := h.parentMessages(rootID)
	assert.Equal(t, int64(2), modelCalls.Load())
	assert.Equal(t, 1, countUserCompletions(messages, "<process_completion>"))
	assert.Equal(t, 1, countToolResultsFor(messages, tool.IDSleep))
	assert.Contains(t, lastToolResultContent(messages, tool.IDSleep), "Sleep interrupted")
	assert.Equal(t, backgroundprocess.StateCompleted,
		waitScenarioProcessState(t, h, process.ID, backgroundprocess.StateCompleted).State)
	assertHarnessTrace(t, "process_interrupts_sleep.json", collector.snapshot(), rootID)
}

func TestHarnessScenario_ProcessCompletionWaitsForForegroundChild(t *testing.T) {
	childRelease := make(chan struct{})
	var parentCalls atomic.Int64
	var missingTaskResult atomic.Bool
	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasUserContaining(messages, "CHILD_PROCESS_BLOCK") {
			<-childRelease

			return &llmwire.Response{Text: "foreground child finished"}
		}

		parentCalls.Add(1)
		if hasUserContaining(messages, "<process_completion>") {
			if !hasToolResultFor(messages, tool.IDTask) {
				missingTaskResult.Store(true)
			}

			return &llmwire.Response{Text: "foreground result preceded process input"}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID: taskCallID, Name: tool.IDTask,
			Arguments: []byte(
				`{"prompt":"CHILD_PROCESS_BLOCK","description":"block","subagent_type":"general"}`,
			),
		}}}
	}

	h := newSubagentHarnessWith(t, respond)
	service := installScenarioProcessService(t, h)
	collector := collectEvents(h.mgr.PubSub().SubscribeAll())
	defer func() {
		closeOnce(childRelease)
		collector.stop()
		h.shutdown()
	}()

	rootID, err := h.mgr.Send(h.ctx, h.projectID, "start foreground process ordering", "fake-model", nil)
	require.NoError(t, err)
	waitForWaitKind(t, collector, rootID, sessionevent.WaitSubagent)
	h.waitUntil("foreground parent parked", func() bool { return !h.mgr.HasActiveLoop(rootID) })
	process := startScenarioProcess(t, service, rootID, rootID, "printf 'queued\\n'")
	waitScenarioProcessState(t, h, process.ID, backgroundprocess.StateCompleted)
	collector.waitFor(t, "process wake remains behind foreground child",
		func(events []controllerapi.SessionNotification) bool {
			waiting := 0
			for _, event := range events {
				if event.SessionID == rootID && event.Notification.Type == sessionevent.NotifyWaiting {
					waiting++
				}
			}

			return waiting == 2
		})
	h.waitUntil("process wake runner parked", func() bool { return !h.mgr.HasActiveLoop(rootID) })

	pending, err := h.mgr.inboxStore.PeekPending(h.ctx, rootID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.InputSourceProcess, pending.Source)
	assert.Equal(t, int64(1), parentCalls.Load(), "process input cannot cross the foreground task call")
	close(childRelease)
	waitForVisibleMessage(t, collector, rootID, "foreground result preceded process input")
	waitForIdleAfterMessage(t, collector, rootID, "foreground result preceded process input")

	messages := h.parentMessages(rootID)
	assert.False(t, missingTaskResult.Load())
	assert.Equal(t, int64(2), parentCalls.Load())
	assert.Equal(t, 1, countToolResultsFor(messages, tool.IDTask))
	assert.Equal(t, 1, countUserCompletions(messages, "<process_completion>"))
	assertHarnessTrace(t, "process_waits_for_foreground.json", collector.snapshot(), rootID)
}

func TestHarnessScenario_ProcessCrashRestartDeliversInterruptedOnce(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "process-crash-restart.db")
	var modelCalls atomic.Int64
	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		modelCalls.Add(1)
		require.True(t, hasUserContaining(messages, "<process_completion>"))
		require.True(t, hasUserContaining(messages, "state: interrupted"))

		return &llmwire.Response{Text: "interrupted process recovered"}
	}

	first := newSubagentHarnessOnDB(t, dbPath, respond, nil)
	root, err := first.sessStore.CreateSession(first.ctx, first.projectID, "fake-model", "", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)
	require.NoError(t, first.sessStore.UpdateSessionStatus(
		first.ctx, root.ID, sessionstore.SessionStatusCompleted,
	))
	outputPath := filepath.Join(t.TempDir(), "interrupted.output")
	require.NoError(t, os.WriteFile(outputPath, []byte("partial output\n"), 0o600))
	now := time.Now().UTC()
	process := backgroundprocess.Process{
		ID: "crashed-process", SessionID: root.ID, RootSessionID: root.ID,
		ToolCallID: "crashed-call", OutputPath: outputPath, CreatedAt: now,
		Deadline: now.Add(time.Minute), AdvertisedAt: &now, OutputSize: 15,
		State: backgroundprocess.StateRunning,
	}
	require.NoError(t, first.mgr.processStore.InsertProcess(first.ctx, process))

	second := newSubagentHarnessOnDB(t, dbPath, respond, nil)
	collector := collectEvents(second.mgr.PubSub().SubscribeAll())
	defer func() {
		collector.stop()
		second.shutdown()
		first.shutdown()
	}()
	require.NoError(t, second.mgr.Start(second.ctx))
	waitForVisibleMessage(t, collector, root.ID, "interrupted process recovered")
	drainScenarioClaims(t, "process_crash_restart.json", newChainController(t, second))
	waitForIdleAfterMessage(t, collector, root.ID, "interrupted process recovered")

	final, err := second.mgr.processStore.GetProcess(second.ctx, process.ID)
	require.NoError(t, err)
	assert.Equal(t, backgroundprocess.StateInterrupted, final.State)
	assert.Equal(t, int64(1), modelCalls.Load())
	var inputs int
	require.NoError(t, second.db.QueryRowContext(second.ctx, `SELECT COUNT(*) FROM session_inbox
		WHERE source = 'process' AND json_extract(attributes, '$.process_id') = ?`, process.ID).Scan(&inputs))
	assert.Equal(t, 1, inputs)
	assertHarnessTrace(t, "process_crash_restart.json", collector.snapshot(), root.ID)
}

func TestProcessCompletionRetainsInputWithoutWakingStoppedOrErroredSession(t *testing.T) {
	for _, status := range []sessionstore.SessionStatus{
		sessionstore.SessionStatusStopped,
		sessionstore.SessionStatusError,
	} {
		t.Run(string(status), func(t *testing.T) {
			var calls atomic.Int64
			h := newSubagentHarnessWith(t, func(string, []llmwire.Message) *llmwire.Response {
				calls.Add(1)

				return &llmwire.Response{Text: "must not run"}
			})
			collector := collectEvents(h.mgr.PubSub().SubscribeAll())
			completionRouted := make(chan struct{})
			service := backgroundprocess.NewService(h.mgr.processStore, backgroundprocess.Options{
				OutputDir: t.TempDir(),
				OnCompletion: func(ctx context.Context, completion backgroundprocess.Completion) {
					h.mgr.routeProcessCompletion(ctx, completion)
					close(completionRouted)
				},
			})
			h.mgr.processSvc = service
			defer func() {
				collector.stop()
				h.shutdown()
			}()

			root, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
			require.NoError(t, err)
			require.NoError(t, h.sessStore.UpdateSessionStatus(h.ctx, root.ID, status))

			process := startScenarioProcess(t, service, root.ID, root.ID, "printf 'parked\\n'")
			<-completionRouted

			assert.False(t, h.mgr.HasActiveLoop(root.ID))
			assert.Zero(t, calls.Load())
			pending, err := h.mgr.inboxStore.PeekPending(h.ctx, root.ID)
			require.NoError(t, err)
			assert.Equal(t, sessionstore.InputSourceProcess, pending.Source)
			assert.Equal(t, process.ID, pending.Attributes["process_id"])
			if status == sessionstore.SessionStatusStopped {
				require.NoError(t, h.mgr.SendToSession(h.ctx, root.ID, "/help"))
				assert.False(t, h.mgr.HasActiveLoop(root.ID))
				head, peekErr := h.mgr.inboxStore.PeekPending(h.ctx, root.ID)
				require.NoError(t, peekErr)
				assert.Equal(t, pending.ID, head.ID)
			}
			assert.Empty(t, collector.snapshot(), "retained completion must emit no controller trace")
		})
	}
}

func countUserCompletions(messages []llmwire.Message, marker string) int {
	count := 0
	for _, message := range messages {
		if message.Role == llmwire.RoleUser && strings.Contains(message.Content, marker) {
			count++
		}
	}

	return count
}
