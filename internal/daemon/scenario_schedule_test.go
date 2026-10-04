package daemon

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

// Parentage, runner admission and status transitions are daemon-owned protocol state.
func TestHarnessModel_ScheduleCapabilityBoundary(t *testing.T) {
	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasToolResultFor(messages, tool.IDSchedule) {
			return &llmwire.Response{Text: "scheduled turn completed"}
		}

		return &llmwire.Response{Text: "ready"}
	}

	h := newSubagentHarnessWith(t, respond)
	defer h.shutdown()
	h.startInboxWake()
	rootID, err := h.mgr.Send(t.Context(), h.projectID, "initialize", "fake-model", nil)
	require.NoError(t, err)
	h.mgr.waitIdle(rootID)
	require.NoError(t, h.mgr.sendToSession(t.Context(), rootID, "/stop"))
	subagentID := createScheduleBoundarySubagent(t, h)

	model := scheduleBoundaryModel{
		rootStatus: sessionstore.SessionStatusStopped, subagentStatus: sessionstore.SessionStatusCompleted,
	}
	commands := []scheduleBoundaryCommand{
		deliverSubagentTick,
		deliverSubagentFresh,
		deliverStoppedRoot,
		stopRootAgain,
		deliverDuplicateRoot,
		deliverPendingResult,
	}

	for _, command := range commands {
		expectedApplied, expectedError := model.step(command)
		actual := applyScheduleBoundaryCommand(t, h, rootID, subagentID, command)
		assert.Equal(t, expectedApplied, actual.applied, "command %d applied", command)
		assert.Equal(t, expectedError, actual.errored, "command %d error", command)
		assert.Equal(t, model.rootStatus, actual.rootStatus, "command %d root status", command)
		assert.Equal(t, model.rootRuns, actual.rootRuns, "command %d root runs", command)
		assert.Equal(t, model.rootPending, actual.rootPending, "command %d root pending", command)
		assert.Equal(t, model.subagentStatus, actual.subagentStatus, "command %d subagent status", command)
		assert.Equal(t, model.subagentMessages, actual.subagentMessages, "command %d subagent messages", command)
	}
}

func TestHarnessScenario_OneShotAckRetrySurvivesDaemonRestartAndRendersOnce(t *testing.T) {
	releaseScheduledRun := make(chan struct{})
	respond := scheduleRestartResponder(releaseScheduledRun)
	dbPath := filepath.Join(t.TempDir(), "schedule-scenario.db")
	workDir := t.TempDir()

	first := newScheduleRestartHarness(t, dbPath, workDir, respond)
	parentID, events := deliverOneShotBeforeRestart(t, first, releaseScheduledRun)
	require.NoError(t, first.close(), "the first daemon must close SQLite before restart")

	second := newScheduleRestartHarness(t, dbPath, workDir, respond)
	retryEvents := retryOneShotAfterRestart(t, second, parentID)
	assertNoRetryPublication(t, retryEvents.snapshot())
	assert.Equal(t, 1, countToolResultsFor(second.parentMessages(parentID), tool.IDSchedule))
	assert.Equal(t, "scheduled work completed", lastAssistantTextDTO(second.parentMessages(parentID)))

	trace := append(events.snapshot(), retryEvents.snapshot()...)
	// Runner-activation echoes are timing around a restart: an announce races
	// the previous runner's drain, a retry runner may exit before executing.
	// Pin the documented symptoms — message and delivery order — only.
	assertHarnessTrace(t, "one_shot_ack_retry_restart.json", dropTransientEvents(trace), parentID)
}

func TestHarnessScenario_SleepProjectsWakeAtAndUserInputInterruptsIt(t *testing.T) {
	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasToolResultFor(messages, tool.IDSleep) {
			return &llmwire.Response{Text: "sleep interruption handled"}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID:        "scenario-sleep",
			Name:      tool.IDSleep,
			Arguments: []byte(`{"duration":"1h","reason":"scenario"}`),
		}}}
	}

	h := newSubagentHarnessWith(t, respond)
	collector := collectEvents(h.mgr.bus.SubscribeAll())
	defer func() {
		collector.stop()
		h.shutdown()
	}()

	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "sleep", "fake-model", nil)
	require.NoError(t, err)
	collector.waitFor(t, "structured sleep wait", func(events []controllerapi.SessionNotification) bool {
		for _, event := range events {
			if event.SessionID != parentID || event.Notification.Type != sessionevent.NotifyWaiting {
				continue
			}

			if len(event.Notification.Waiting) != 1 {
				return false
			}
			wait := event.Notification.Waiting[0]

			return wait.Kind == sessionevent.WaitSleep && wait.WakeAt != nil && wait.ChildID == 0
		}

		return false
	})

	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, parentID, "interrupt now"))
	waitForVisibleMessage(t, collector, parentID, "sleep interruption handled")
	waitForIdleAfterMessage(t, collector, parentID, "sleep interruption handled")

	messages := h.parentMessages(parentID)
	require.NoError(t, llm.ValidateToolPairing(messages))
	assert.Equal(t, 1, countToolResultsFor(messages, tool.IDSleep))
	assert.Contains(t, lastToolResultContent(messages, tool.IDSleep), "Sleep interrupted")

	schedules, err := h.schedStore.ListSchedules(h.ctx, parentID)
	require.NoError(t, err)
	assert.Empty(t, schedules)

	assertHarnessTrace(t, "sleep_interrupted_by_input.json", collector.snapshot(), parentID)
}

// This is the composition-boundary contract, not another executor unit test:
// real schedule.Executor calls the daemon's producer-owned SessionSender,
// daemon queues an exact result, and session attaches it to the original call.
func TestIntegration_SchedulerWakesExactSleepThroughDaemonQueue(t *testing.T) {
	const sleepCallID = "sleep-call-scheduler-boundary"

	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasToolResultFor(msgs, tool.IDSleep) {
			return &llmwire.Response{Text: "timer handled"}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID:        sleepCallID,
			Name:      tool.IDSleep,
			Arguments: []byte(`{"duration":"1ms","reason":"boundary test"}`),
		}}}
	}

	h := newSubagentHarnessWith(t, respond)
	defer h.shutdown()

	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "sleep briefly", "fake-model", nil)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		schedules, listErr := h.schedStore.ListSchedules(h.ctx, parentID)

		return listErr == nil && len(schedules) == 1 && !h.mgr.HasActiveLoop(parentID)
	}, 5*time.Second, 10*time.Millisecond, "sleep must be durable before the scheduler fires")

	executor := schedule.NewExecutor(h.schedStore, h.mgr)
	executor.Start(h.ctx)
	defer executor.Stop()

	require.Eventually(t, func() bool {
		schedules, listErr := h.schedStore.ListSchedules(h.ctx, parentID)
		if listErr != nil || len(schedules) != 0 {
			return false
		}

		for _, msg := range h.parentMessages(parentID) {
			if msg.Role == llmwire.RoleTool && msg.ToolCallID == sleepCallID {
				return !h.mgr.HasActiveLoop(parentID)
			}
		}

		return false
	}, 5*time.Second, 10*time.Millisecond, "scheduler must commit the exact result before removing its ledger")

	msgs := h.parentMessages(parentID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Equal(t, 1, countAssistantToolCallsFor(msgs, tool.IDSleep))
	assert.Equal(t, 1, countToolResultsFor(msgs, tool.IDSleep))

	var result *llmwire.Message
	for i := range msgs {
		if msgs[i].Role == llmwire.RoleTool && msgs[i].ToolName == tool.IDSleep {
			result = &msgs[i]
			break
		}
	}

	require.NotNil(t, result)
	assert.Equal(t, sleepCallID, result.ToolCallID)
	assert.Contains(t, result.Content, "Sleep completed")

	schedules, err := h.schedStore.ListSchedules(h.ctx, parentID)
	require.NoError(t, err)
	assert.Empty(t, schedules, "accepted one-shot must be removed only after transcript delivery")
}

func TestIntegration_UserInterruptCancelsSleepWithoutDeletingStandaloneOneShot(t *testing.T) {
	const sleepCallID = "sleep-call-user-interrupt"

	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasToolResultFor(msgs, tool.IDSleep) {
			return &llmwire.Response{Text: "interrupt handled"}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID:        sleepCallID,
			Name:      tool.IDSleep,
			Arguments: []byte(`{"duration":"1h","reason":"boundary test"}`),
		}}}
	}

	h := newSubagentHarnessWith(t, respond)
	defer h.shutdown()

	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "sleep until interrupted", "fake-model", nil)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		schedules, listErr := h.schedStore.ListSchedules(h.ctx, parentID)

		return listErr == nil && len(schedules) == 1 && !h.mgr.HasActiveLoop(parentID)
	}, 5*time.Second, 10*time.Millisecond, "sleep must suspend with one durable timer")

	standaloneAt := time.Now().Add(2 * time.Hour).UTC()
	_, err = h.schedStore.AddSchedule(
		h.ctx, parentID, "", &standaloneAt, "standalone future input", false,
	)
	require.NoError(t, err)

	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, parentID, "interrupt now"))
	h.mgr.waitIdle(parentID)

	messages := h.parentMessages(parentID)
	require.NoError(t, llm.ValidateToolPairing(messages))
	assert.Equal(t, 1, countToolResultsFor(messages, tool.IDSleep))
	assert.Contains(t, lastToolResultContent(messages, tool.IDSleep), "Sleep interrupted")
	assert.Equal(t, "interrupt handled", lastAssistantTextDTO(messages))

	schedules, err := h.schedStore.ListSchedules(h.ctx, parentID)
	require.NoError(t, err)
	require.Len(t, schedules, 1, "interrupt must remove only the pending sleep timer")
	assert.Equal(t, "standalone future input", schedules[0].InputMessage())
}

func TestIntegration_StandaloneOneShotFlowsThroughExecutorAndDaemonQueue(t *testing.T) {
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasToolResultFor(msgs, tool.IDSchedule) {
			return &llmwire.Response{Text: "one-shot handled"}
		}

		return &llmwire.Response{Text: "ready"}
	}

	h := newSubagentHarnessWith(t, respond)
	defer h.shutdown()

	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "initialize", "fake-model", nil)
	require.NoError(t, err)
	h.mgr.waitIdle(parentID)

	due := time.Now().Add(-time.Minute).UTC()
	_, err = h.schedStore.AddSchedule(
		h.ctx, parentID, "", &due, "one-time scheduled work", false,
	)
	require.NoError(t, err)

	executor := schedule.NewExecutor(h.schedStore, h.mgr)
	executor.Start(h.ctx)
	defer executor.Stop()

	require.Eventually(t, func() bool {
		schedules, listErr := h.schedStore.ListSchedules(h.ctx, parentID)
		if listErr != nil || len(schedules) != 0 || h.mgr.HasActiveLoop(parentID) {
			return false
		}

		return lastAssistantTextDTO(h.parentMessages(parentID)) == "one-shot handled"
	}, 5*time.Second, 10*time.Millisecond)

	messages := h.parentMessages(parentID)
	require.NoError(t, llm.ValidateToolPairing(messages))
	assert.Equal(t, 1, countToolResultsFor(messages, tool.IDSchedule))
	assert.Contains(t, lastToolResultContent(messages, tool.IDSchedule), "one-time scheduled work")
}

func TestIntegration_OneShotAckFailureRedeliversWithoutDuplicateTranscriptOrPublication(t *testing.T) {
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasToolResultFor(msgs, tool.IDSchedule) {
			return &llmwire.Response{Text: "one-shot handled once"}
		}

		return &llmwire.Response{Text: "ready"}
	}

	h := newSubagentHarnessWith(t, respond)
	defer h.shutdown()

	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "initialize", "fake-model", nil)
	require.NoError(t, err)
	h.mgr.waitIdle(parentID)

	due := time.Now().Add(-time.Minute).UTC()
	_, err = h.schedStore.AddSchedule(
		h.ctx, parentID, "", &due, "one-time retry-safe work", false,
	)
	require.NoError(t, err)

	sub := h.mgr.bus.Subscribe(parentID)
	defer h.mgr.bus.Unsubscribe(parentID, sub)

	flaky := &failFirstRemoveScheduleStore{
		Store: h.schedStore, attempted: make(chan struct{}),
	}
	first := schedule.NewExecutor(flaky, h.mgr)
	first.Start(h.ctx)
	select {
	case <-flaky.attempted:
	case <-time.After(5 * time.Second):
		t.Fatal("first executor did not reach the failing acknowledgement")
	}
	first.Stop()
	h.mgr.waitIdle(parentID)

	remaining, err := h.schedStore.ListSchedules(h.ctx, parentID)
	require.NoError(t, err)
	require.Len(t, remaining, 1, "failed producer ack must leave the one-shot retryable")

	second := schedule.NewExecutor(h.schedStore, h.mgr)
	second.Start(h.ctx)
	require.Eventually(t, func() bool {
		schedules, listErr := h.schedStore.ListSchedules(h.ctx, parentID)
		return listErr == nil && len(schedules) == 0 && !h.mgr.HasActiveLoop(parentID)
	}, 5*time.Second, 10*time.Millisecond)
	second.Stop()

	messages := h.parentMessages(parentID)
	require.NoError(t, llm.ValidateToolPairing(messages))
	assert.Equal(t, 1, countToolResultsFor(messages, tool.IDSchedule))
	assert.Equal(t, "one-shot handled once", lastAssistantTextDTO(messages))

	scheduledPublications := 0
	for {
		select {
		case notification := <-sub:
			if notification.Type == sessionevent.NotifyInputReceived && notification.Source == "scheduler" {
				scheduledPublications++
			}
		default:
			assert.Equal(t, 1, scheduledPublications, "ack retry must not republish accepted input")
			return
		}
	}
}

func TestIntegration_FreshScheduleDuplicateDoesNotResetOrRunTwice(t *testing.T) {
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "fresh scheduled work") {
			return &llmwire.Response{Text: "fresh handled once"}
		}

		return &llmwire.Response{Text: "ready"}
	}

	h := newSubagentHarnessWith(t, respond)
	defer h.shutdown()

	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "initialize", "fake-model", nil)
	require.NoError(t, err)
	h.mgr.waitIdle(parentID)

	const deliveryID = "schedule:cron:23:20260814T1200Z"
	applied, err := enqueueScheduledInput(h.ctx, h.mgr.store, parentID, deliveryID, "fresh scheduled work", true)
	require.NoError(t, err)
	assert.True(t, applied)
	h.waitUntil("fresh schedule completed", func() bool {
		return countMessageContentContaining(h.parentMessages(parentID), "fresh handled once") == 2
	})
	h.mgr.waitIdle(parentID)

	applied, err = enqueueScheduledInput(h.ctx, h.mgr.store, parentID, deliveryID, "fresh scheduled work", true)
	require.NoError(t, err)
	assert.False(t, applied)
	h.mgr.waitIdle(parentID)

	messages := h.parentMessages(parentID)
	require.NoError(t, llm.ValidateToolPairing(messages))
	assert.Equal(t, 1, countMessageContentContaining(messages, "fresh scheduled work"))
	// The confirmed stop publishes one answer; its hidden candidate row stays
	// in the transcript with the same text.
	assert.Equal(t, 2, countMessageContentContaining(messages, "fresh handled once"))
}

func TestHarnessScenario_StoppedRootScheduleStartsOneTurn(t *testing.T) {
	cases := []stoppedRootScheduleCase{
		{
			name:   "normal",
			prompt: "scheduled task",
			answer: "scheduled turn completed",
			trace:  "stopped_root_schedule_turn.json",
		},
		{
			name:   "fresh",
			prompt: "fresh scheduled task",
			answer: "fresh scheduled turn completed",
			trace:  "stopped_root_fresh_schedule_turn.json",
			fresh:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runStoppedRootScheduleScenario(t, tc)
		})
	}
}

func TestScheduledDeliveryToSubagentIsAcknowledgedWithoutMutation(t *testing.T) {
	tests := []struct {
		name    string
		deliver func(*svc, int64) (bool, error)
	}{
		{
			name: "normal",
			deliver: func(mgr *svc, sessionID int64) (bool, error) {
				return enqueueScheduledInput(

					t.Context(), mgr.store,

					sessionID,
					"schedule:test:normal",
					"legacy task",
					false,
				)
			},
		},
		{
			name: "fresh",
			deliver: func(mgr *svc, sessionID int64) (bool, error) {
				return enqueueScheduledInput(

					t.Context(), mgr.store,

					sessionID,
					"schedule:test:fresh",
					"legacy fresh task",
					true,
				)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newSubagentHarnessWith(t, trivialRespond)
			defer h.shutdown()

			childID := createScheduleBoundarySubagent(t, h)
			before := h.parentMessages(childID)

			applied, err := tt.deliver(h.mgr, childID)
			require.NoError(t, err)
			assert.False(t, applied)
			assert.False(t, h.mgr.HasActiveLoop(childID))
			assert.Equal(t, before, h.parentMessages(childID))

			rec, err := h.sessStore.GetSession(t.Context(), childID)
			require.NoError(t, err)
			assert.Equal(t, sessionstore.SessionStatusCompleted, rec.Status)
			assert.Zero(t, rec.Iteration)
		})
	}
}

func TestIntegration_LegacySubagentOneShotIsDiscardedWithoutRun(t *testing.T) {
	h := newSubagentHarnessWith(t, trivialRespond)
	defer h.shutdown()

	childID := createScheduleBoundarySubagent(t, h)
	due := time.Now().Add(-time.Minute).UTC()
	_, err := h.schedStore.AddSchedule(t.Context(), childID, "", &due, "legacy task", false)
	require.NoError(t, err)

	executor := schedule.NewExecutor(h.schedStore, h.mgr)
	executor.Start(t.Context())
	defer executor.Stop()

	require.Eventually(t, func() bool {
		entries, listErr := h.schedStore.ListSchedules(t.Context(), childID)
		return listErr == nil && len(entries) == 0
	}, 5*time.Second, 10*time.Millisecond)
	executor.Stop()

	assert.False(t, h.mgr.HasActiveLoop(childID))
	assert.Empty(t, h.parentMessages(childID))
	rec, err := h.sessStore.GetSession(t.Context(), childID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.SessionStatusCompleted, rec.Status)
	assert.Zero(t, rec.Iteration)
}

func TestIntegration_LegacySubagentCronOccurrencesAreAcknowledgedWithoutRun(t *testing.T) {
	tests := []struct {
		name  string
		fresh bool
	}{
		{name: "normal"},
		{name: "fresh", fresh: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newSubagentHarnessWith(t, trivialRespond)
			defer h.shutdown()

			childID := createScheduleBoundarySubagent(t, h)
			before := h.parentMessages(childID)
			collector := collectEvents(h.mgr.bus.SubscribeAll())
			defer collector.stop()

			entry, err := h.schedStore.AddSchedule(t.Context(), childID, "* * * * *", nil, "legacy task", tt.fresh)
			require.NoError(t, err)

			executor := schedule.NewExecutor(h.schedStore, h.mgr)
			executor.Start(t.Context())
			defer executor.Stop()

			require.Eventually(t, func() bool {
				schedules, listErr := h.schedStore.ListSchedules(t.Context(), childID)
				return listErr == nil && len(schedules) == 1 && schedules[0].ID() == entry.ID() &&
					schedules[0].LastFiredAt() != nil
			}, 5*time.Second, 10*time.Millisecond)
			executor.Stop()

			assert.False(t, h.mgr.HasActiveLoop(childID))
			assert.Equal(t, before, h.parentMessages(childID))
			assert.Empty(t, collector.snapshot(), "an acknowledged legacy occurrence is not published")
			rec, err := h.sessStore.GetSession(t.Context(), childID)
			require.NoError(t, err)
			assert.Equal(t, sessionstore.SessionStatusCompleted, rec.Status)
			assert.Zero(t, rec.Iteration)
		})
	}
}

// A scheduled turn bypasses the inbox: its announcement and its narration form
// a new generation even though nothing was promoted.
func TestHarnessScenario_ScheduledTurnChain(t *testing.T) {
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasToolResultFor(msgs, "schedule") {
			return &llmwire.Response{Text: "Report ready."}
		}

		return &llmwire.Response{Text: "Hi there."}
	}

	h := newSubagentHarnessWith(t, respond)
	defer h.shutdown()

	collector := collectEvents(h.mgr.bus.SubscribeAll())
	defer collector.stop()

	h.startInboxWake()
	root, err := h.mgr.Send(h.ctx, h.projectID, "hello", "fake-model", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)
	h.waitUntil("first turn completed", func() bool {
		record, loadErr := h.sessStore.GetSession(h.ctx, root)

		return loadErr == nil && record.Status == sessionstore.SessionStatusCompleted
	})
	// Wait for the loop teardown so the scheduled turn deterministically
	// re-announces the session instead of racing the runner cleanup.
	h.waitUntil("first runner gone", func() bool { return !h.mgr.HasActiveLoop(root) })

	delivered, err := enqueueScheduledInput(

		h.ctx, h.mgr.store,

		root,
		"delivery-chain-1",
		"produce the weekly report",
		false,
	)
	require.NoError(t, err)
	require.True(t, delivered)

	waitForVisibleMessage(t, collector, root, "Report ready.")

	controller := newChainController(t, h)
	drainScenarioClaims(t, "scheduled_turn_chain.json", controller)
	waitForIdleAfterMessage(t, collector, root, "Report ready.")

	assertHarnessTrace(t, "scheduled_turn_chain.json", collector.snapshot(), root)
}
