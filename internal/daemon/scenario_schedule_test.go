package daemon

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/sessionbus"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

// Parentage, runner admission and status transitions are daemon-owned protocol state.
func TestHarnessModel_ScheduleCapabilityBoundary(t *testing.T) {
	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasToolResultFor(messages, tool.IDSchedule) {
			return textReply("scheduled turn completed")
		}
		return textReply("ready")
	}
	h := newHarness(t, harnessOptions{respond: respond})
	h.startInboxWake()
	rootID, err := h.mgr.Send(t.Context(), h.projectID, "initialize", "fake-model", nil)
	require.NoError(t, err)
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(rootID) })
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
	assert.Equal(t, 1, countToolResultsFor(second.messages(parentID), tool.IDSchedule))
	assert.Equal(t, "scheduled work completed", lastAssistantTextDTO(second.messages(parentID)))
	trace := append(events.snapshot(), retryEvents.snapshot()...)
	// Restart announcements race runner drains; pin message and delivery order instead of activation timing noise.
	assertHarnessTrace(t, "one_shot_ack_retry_restart.json", dropTransientEvents(trace), parentID)
}

func TestHarnessScenario_SleepProjectsWakeAtAndUserInputInterruptsIt(t *testing.T) {
	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasToolResultFor(messages, tool.IDSleep) {
			return textReply("sleep interruption handled")
		}
		return callReply("scenario-sleep", tool.IDSleep, `{"duration":"1h","reason":"scenario"}`)
	}
	h := newHarness(t, harnessOptions{respond: respond})
	collector := collectEvents(t, h.mgr.bus.SubscribeAll())
	defer collector.stop()
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
	collector.waitMessage(parentID, "sleep interruption handled")
	collector.waitIdleAfter(parentID, "sleep interruption handled")
	messages := h.messages(parentID)
	require.NoError(t, llm.ValidateToolPairing(messages))
	assert.Equal(t, 1, countToolResultsFor(messages, tool.IDSleep))
	assert.Contains(t, scheduleLastToolResultContent(messages, tool.IDSleep), "Sleep interrupted")
	schedules, err := h.schedules.ListSchedules(h.ctx, parentID)
	require.NoError(t, err)
	assert.Empty(t, schedules)
	assertHarnessTrace(t, "sleep_interrupted_by_input.json", collector.snapshot(), parentID)
}

// The real executor, daemon sender and session must attach one exact scheduled result to its original tool call.
func TestIntegration_SchedulerWakesExactSleepThroughDaemonQueue(t *testing.T) {
	const sleepCallID = "sleep-call-scheduler-boundary"
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasToolResultFor(msgs, tool.IDSleep) {
			return textReply("timer handled")
		}
		return callReply(sleepCallID, tool.IDSleep, `{"duration":"1ms","reason":"boundary test"}`)
	}
	h := newHarness(t, harnessOptions{respond: respond})
	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "sleep briefly", "fake-model", nil)
	require.NoError(t, err)
	h.waitUntil("sleep timer committed", func() bool {
		schedules, listErr := h.schedules.ListSchedules(h.ctx, parentID)
		return listErr == nil && len(schedules) == 1 && !h.mgr.HasActiveLoop(parentID)
	})
	executor := schedule.NewExecutor(h.schedules, h.mgr)
	executor.Start(h.ctx)
	defer executor.Stop()
	h.waitUntil("sleep result committed", func() bool {
		schedules, listErr := h.schedules.ListSchedules(h.ctx, parentID)
		if listErr != nil || len(schedules) != 0 {
			return false
		}
		for _, msg := range h.messages(parentID) {
			if msg.Role == llmwire.RoleTool && msg.ToolCallID == sleepCallID {
				return !h.mgr.HasActiveLoop(parentID)
			}
		}
		return false
	})
	msgs := h.messages(parentID)
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
	schedules, err := h.schedules.ListSchedules(h.ctx, parentID)
	require.NoError(t, err)
	assert.Empty(t, schedules, "accepted one-shot must be removed only after transcript delivery")
}

func TestIntegration_UserInterruptCancelsSleepWithoutDeletingStandaloneOneShot(t *testing.T) {
	const sleepCallID = "sleep-call-user-interrupt"
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasToolResultFor(msgs, tool.IDSleep) {
			return textReply("interrupt handled")
		}
		return callReply(sleepCallID, tool.IDSleep, `{"duration":"1h","reason":"boundary test"}`)
	}
	h := newHarness(t, harnessOptions{respond: respond})
	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "sleep until interrupted", "fake-model", nil)
	require.NoError(t, err)
	h.waitUntil("sleep suspended", func() bool {
		schedules, listErr := h.schedules.ListSchedules(h.ctx, parentID)
		return listErr == nil && len(schedules) == 1 && !h.mgr.HasActiveLoop(parentID)
	})
	standaloneAt := time.Now().Add(2 * time.Hour).UTC()
	_, err = h.schedules.AddSchedule(h.ctx, parentID, "", &standaloneAt, "standalone future input", false)
	require.NoError(t, err)
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, parentID, "interrupt now"))
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(parentID) })
	messages := h.messages(parentID)
	require.NoError(t, llm.ValidateToolPairing(messages))
	assert.Equal(t, 1, countToolResultsFor(messages, tool.IDSleep))
	assert.Contains(t, scheduleLastToolResultContent(messages, tool.IDSleep), "Sleep interrupted")
	assert.Equal(t, "interrupt handled", lastAssistantTextDTO(messages))
	schedules, err := h.schedules.ListSchedules(h.ctx, parentID)
	require.NoError(t, err)
	require.Len(t, schedules, 1, "interrupt must remove only the pending sleep timer")
	assert.Equal(t, "standalone future input", schedules[0].InputMessage())
}

func TestIntegration_StandaloneOneShotFlowsThroughExecutorAndDaemonQueue(t *testing.T) {
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasToolResultFor(msgs, tool.IDSchedule) {
			return textReply("one-shot handled")
		}
		return textReply("ready")
	}
	h := newHarness(t, harnessOptions{respond: respond})
	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "initialize", "fake-model", nil)
	require.NoError(t, err)
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(parentID) })
	due := time.Now().Add(-time.Minute).UTC()
	_, err = h.schedules.AddSchedule(h.ctx, parentID, "", &due, "one-time scheduled work", false)
	require.NoError(t, err)
	executor := schedule.NewExecutor(h.schedules, h.mgr)
	executor.Start(h.ctx)
	defer executor.Stop()
	h.waitUntil("one-shot delivered", func() bool {
		schedules, listErr := h.schedules.ListSchedules(h.ctx, parentID)
		if listErr != nil || len(schedules) != 0 || h.mgr.HasActiveLoop(parentID) {
			return false
		}
		return lastAssistantTextDTO(h.messages(parentID)) == "one-shot handled"
	})
	messages := h.messages(parentID)
	require.NoError(t, llm.ValidateToolPairing(messages))
	assert.Equal(t, 1, countToolResultsFor(messages, tool.IDSchedule))
	assert.Contains(t, scheduleLastToolResultContent(messages, tool.IDSchedule), "one-time scheduled work")
}

func TestIntegration_OneShotAckFailureRedeliversWithoutDuplicateTranscriptOrPublication(t *testing.T) {
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasToolResultFor(msgs, tool.IDSchedule) {
			return textReply("one-shot handled once")
		}
		return textReply("ready")
	}
	h := newHarness(t, harnessOptions{respond: respond})
	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "initialize", "fake-model", nil)
	require.NoError(t, err)
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(parentID) })
	due := time.Now().Add(-time.Minute).UTC()
	_, err = h.schedules.AddSchedule(h.ctx, parentID, "", &due, "one-time retry-safe work", false)
	require.NoError(t, err)
	sub := h.mgr.bus.Subscribe(parentID)
	defer h.mgr.bus.Unsubscribe(parentID, sub)
	flaky := &failFirstRemoveScheduleStore{Store: h.schedules, attempted: make(chan struct{})}
	first := schedule.NewExecutor(flaky, h.mgr)
	first.Start(h.ctx)
	select {
	case <-flaky.attempted:
	case <-time.After(5 * time.Second):
		t.Fatal("first executor did not reach the failing acknowledgement")
	}
	first.Stop()
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(parentID) })
	remaining, err := h.schedules.ListSchedules(h.ctx, parentID)
	require.NoError(t, err)
	require.Len(t, remaining, 1, "failed producer ack must leave the one-shot retryable")
	second := schedule.NewExecutor(h.schedules, h.mgr)
	second.Start(h.ctx)
	h.waitUntil("one-shot retry acknowledged", func() bool {
		schedules, listErr := h.schedules.ListSchedules(h.ctx, parentID)
		return listErr == nil && len(schedules) == 0 && !h.mgr.HasActiveLoop(parentID)
	})
	second.Stop()
	messages := h.messages(parentID)
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
			return textReply("fresh handled once")
		}
		return textReply("ready")
	}
	h := newHarness(t, harnessOptions{respond: respond})
	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "initialize", "fake-model", nil)
	require.NoError(t, err)
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(parentID) })
	const deliveryID = "schedule:cron:23:20260814T1200Z"
	applied, err := enqueueScheduledInput(h.ctx, h.mgr.store, parentID, deliveryID, "fresh scheduled work", true)
	require.NoError(t, err)
	assert.True(t, applied)
	h.waitUntil("fresh schedule completed", func() bool {
		return countMessageContentContaining(h.messages(parentID), "fresh handled once") == 2
	})
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(parentID) })
	applied, err = enqueueScheduledInput(h.ctx, h.mgr.store, parentID, deliveryID, "fresh scheduled work", true)
	require.NoError(t, err)
	assert.False(t, applied)
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(parentID) })
	messages := h.messages(parentID)
	require.NoError(t, llm.ValidateToolPairing(messages))
	assert.Equal(t, 1, countMessageContentContaining(messages, "fresh scheduled work"))
	// The confirmed stop publishes one answer; its hidden candidate row stays in the transcript with the same text.
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
					t.Context(), mgr.store, sessionID, "schedule:test:normal", "legacy task", false,
				)
			},
		},
		{
			name: "fresh",
			deliver: func(mgr *svc, sessionID int64) (bool, error) {
				return enqueueScheduledInput(
					t.Context(), mgr.store, sessionID, "schedule:test:fresh", "legacy fresh task", true,
				)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, harnessOptions{respond: trivialRespond})
			childID := createScheduleBoundarySubagent(t, h)
			before := h.messages(childID)
			applied, err := tt.deliver(h.mgr, childID)
			require.NoError(t, err)
			assert.False(t, applied)
			assert.False(t, h.mgr.HasActiveLoop(childID))
			assert.Equal(t, before, h.messages(childID))
			rec := h.session(childID)
			assert.Equal(t, sessionstore.SessionStatusCompleted, rec.Status)
			assert.Zero(t, rec.Iteration)
		})
	}
}

func TestIntegration_LegacySubagentOneShotIsDiscardedWithoutRun(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: trivialRespond})
	childID := createScheduleBoundarySubagent(t, h)
	due := time.Now().Add(-time.Minute).UTC()
	_, err := h.schedules.AddSchedule(t.Context(), childID, "", &due, "legacy task", false)
	require.NoError(t, err)
	executor := schedule.NewExecutor(h.schedules, h.mgr)
	executor.Start(t.Context())
	defer executor.Stop()
	h.waitUntil("legacy one-shot discarded", func() bool {
		entries, listErr := h.schedules.ListSchedules(t.Context(), childID)
		return listErr == nil && len(entries) == 0
	})
	executor.Stop()
	assert.False(t, h.mgr.HasActiveLoop(childID))
	assert.Empty(t, h.messages(childID))
	rec := h.session(childID)
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
			h := newHarness(t, harnessOptions{respond: trivialRespond})
			childID := createScheduleBoundarySubagent(t, h)
			before := h.messages(childID)
			collector := collectEvents(t, h.mgr.bus.SubscribeAll())
			defer collector.stop()
			entry, err := h.schedules.AddSchedule(t.Context(), childID, "* * * * *", nil, "legacy task", tt.fresh)
			require.NoError(t, err)
			executor := schedule.NewExecutor(h.schedules, h.mgr)
			executor.Start(t.Context())
			defer executor.Stop()
			h.waitUntil("legacy cron acknowledged", func() bool {
				schedules, listErr := h.schedules.ListSchedules(t.Context(), childID)
				return listErr == nil && len(schedules) == 1 && schedules[0].ID() == entry.ID() &&
					schedules[0].LastFiredAt() != nil
			})
			executor.Stop()
			assert.False(t, h.mgr.HasActiveLoop(childID))
			assert.Equal(t, before, h.messages(childID))
			assert.Empty(t, collector.snapshot(), "an acknowledged legacy occurrence is not published")
			rec := h.session(childID)
			assert.Equal(t, sessionstore.SessionStatusCompleted, rec.Status)
			assert.Zero(t, rec.Iteration)
		})
	}
}

// Scheduled announcements and narration start a new generation without inbox promotion.
func TestHarnessScenario_ScheduledTurnChain(t *testing.T) {
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasToolResultFor(msgs, "schedule") {
			return textReply("Report ready.")
		}
		return textReply("Hi there.")
	}
	h := newHarness(t, harnessOptions{respond: respond})

	collector := collectEvents(t, h.mgr.bus.SubscribeAll())
	defer collector.stop()
	h.startInboxWake()
	root, err := h.mgr.Send(h.ctx, h.projectID, "hello", "fake-model", managerAttrs(scenarioManagerID))
	require.NoError(t, err)
	h.waitUntil("first turn completed", func() bool {
		record, loadErr := h.store.GetSession(h.ctx, root)
		return loadErr == nil && record.Status == sessionstore.SessionStatusCompleted
	})
	// Wait for teardown so the scheduled turn deterministically re-announces its session.
	h.waitUntil("first runner gone", func() bool { return !h.mgr.HasActiveLoop(root) })
	delivered, err := enqueueScheduledInput(
		h.ctx, h.mgr.store, root, "delivery-chain-1", "produce the weekly report", false,
	)
	require.NoError(t, err)
	require.True(t, delivered)
	collector.waitMessage(root, "Report ready.")
	controller := newChainController(t, h)
	drainScenarioClaims(t, "scheduled_turn_chain.json", controller)
	collector.waitIdleAfter(root, "Report ready.")
	assertHarnessTrace(t, "scheduled_turn_chain.json", collector.snapshot(), root)
}

func scheduleLastToolResultContent(msgs []llmwire.Message, toolName string) string {
	for _, v := range slices.Backward(msgs) {
		if v.Role == llmwire.RoleTool && v.ToolName == toolName {
			return v.Content
		}
	}
	return ""
}

type failFirstRemoveScheduleStore struct {
	schedule.Store
	once      sync.Once
	attempted chan struct{}
}

func (s *failFirstRemoveScheduleStore) RemoveSchedule(ctx context.Context, id int64) error {
	failed := false
	s.once.Do(func() {
		failed = true
		close(s.attempted)
	})
	if failed {
		return assert.AnError
	}
	return s.Store.RemoveSchedule(ctx, id)
}

type scheduleBoundaryCommand uint8

const (
	deliverSubagentTick scheduleBoundaryCommand = iota
	deliverSubagentFresh
	deliverStoppedRoot
	stopRootAgain
	deliverDuplicateRoot
	deliverPendingResult
)

type scheduleBoundaryModel struct {
	rootStatus       sessionstore.SessionStatus
	rootRuns         int
	rootClaimed      bool
	rootPending      int
	subagentStatus   sessionstore.SessionStatus
	subagentMessages int
}

type scheduleBoundaryObservation struct {
	applied          bool
	errored          bool
	rootStatus       sessionstore.SessionStatus
	rootRuns         int
	rootPending      int
	subagentStatus   sessionstore.SessionStatus
	subagentMessages int
}

func (m *scheduleBoundaryModel) step(command scheduleBoundaryCommand) (bool, bool) {
	switch command {
	case deliverSubagentTick, deliverSubagentFresh:
		return false, false
	case deliverStoppedRoot:
		if m.rootClaimed {
			return false, false
		}

		m.rootClaimed = true
		m.rootStatus = sessionstore.SessionStatusCompleted
		m.rootRuns++
		return true, false
	case stopRootAgain:
		m.rootStatus = sessionstore.SessionStatusStopped
		return false, false
	case deliverDuplicateRoot:
		return false, false
	case deliverPendingResult:
		m.rootPending++
		return true, false
	default:
		panic("unknown schedule boundary command")
	}
}

func applyScheduleBoundaryCommand(
	t *testing.T,
	h *harness,
	rootID, subagentID int64,
	command scheduleBoundaryCommand,
) scheduleBoundaryObservation {
	t.Helper()
	applied, err := executeScheduleBoundaryCommand(t, h, rootID, subagentID, command)
	wantPending := 0
	if command == deliverPendingResult {
		wantPending = 1
	}
	h.waitUntil("applyScheduleBoundaryCommand", func() bool {
		pending, pendingErr := h.store.ListPending(t.Context(), rootID)
		return pendingErr == nil && len(pending) == wantPending &&
			!h.mgr.HasActiveLoop(rootID) && !h.mgr.HasActiveLoop(subagentID)
	})
	root := h.session(rootID)
	subagent := h.session(subagentID)
	pending, pendingErr := h.store.ListPending(t.Context(), rootID)
	require.NoError(t, pendingErr)
	return scheduleBoundaryObservation{
		applied:          applied,
		errored:          err != nil,
		rootStatus:       root.Status,
		rootRuns:         countToolResultsFor(h.messages(rootID), tool.IDSchedule),
		rootPending:      len(pending),
		subagentStatus:   subagent.Status,
		subagentMessages: len(h.messages(subagentID)),
	}
}

func executeScheduleBoundaryCommand(
	t *testing.T,
	h *harness,
	rootID, subagentID int64,
	command scheduleBoundaryCommand,
) (bool, error) {
	t.Helper()
	switch command {
	case deliverSubagentTick:
		return enqueueScheduledInput(
			t.Context(), h.mgr.store, subagentID, "schedule:model:subagent-tick", "legacy task", false,
		)
	case deliverSubagentFresh:
		return enqueueScheduledInput(
			t.Context(), h.mgr.store, subagentID, "schedule:model:subagent-fresh", "legacy fresh task", true,
		)
	case deliverStoppedRoot, deliverDuplicateRoot:
		return enqueueScheduledInput(t.Context(), h.mgr.store, rootID, "schedule:model:root", "scheduled task", false)
	case stopRootAgain:
		return false, h.mgr.sendToSession(t.Context(), rootID, "/stop")
	case deliverPendingResult:
		applied, err := enqueueCallResult(
			t.Context(), h.mgr.store, rootID, "missing-call", tool.IDSleep, "must stay stopped",
		)

		return applied, err
	default:
		t.Fatalf("unknown command %d", command)

		return false, nil
	}
}

type scheduleRestartHarness struct {
	*harness
	db        *sql.DB
	closeOnce sync.Once
	closeErr  error
}

type scheduleRunningObserver struct {
	source       sessionbus.Source
	subscription <-chan controllerapi.SessionNotification
	running      chan struct{}
	done         chan struct{}
	stopped      chan struct{}
	closeOnce    sync.Once
}

type orderedScheduleSender struct {
	schedule.SessionSender
	running <-chan struct{}
	done    <-chan struct{}
}

func dropTransientEvents(events []controllerapi.SessionNotification) []controllerapi.SessionNotification {
	kept := make([]controllerapi.SessionNotification, 0, len(events))
	for _, event := range events {
		if event.Notification.Type == sessionevent.NotifyStateChanged ||
			event.Notification.Type == sessionevent.NotifySessionCreated ||
			event.Notification.Message == "⚠️ Could not verify background work before releasing the active budget; the budget remains armed." {
			continue
		}
		kept = append(kept, event)
	}
	return kept
}

func deliverOneShotBeforeRestart(
	t *testing.T,
	h *scheduleRestartHarness,
	release chan<- struct{},
) (int64, *eventCollector) {
	t.Helper()
	events := collectEvents(t, h.mgr.bus.SubscribeManager("telegram-main"))
	t.Cleanup(events.stop)
	parentID := createScheduleSession(t, h, events)
	flaky := addFlakyDueOneShot(t, h, parentID)
	observer := newScheduleRunningObserver(t, h.mgr.bus)
	sender := &orderedScheduleSender{SessionSender: h.mgr, running: observer.running, done: observer.done}
	executor := schedule.NewExecutor(flaky, sender)
	executor.Start(h.ctx)
	t.Cleanup(executor.Stop)
	waitForScheduledInput(t, events, parentID)
	requireSignal(t, flaky.attempted)
	close(release)
	events.waitMessage(parentID, "scheduled work completed")
	executor.Stop()
	requireOneShotRemainsRetryable(t, h, parentID)
	return parentID, events
}

func createScheduleSession(t *testing.T, h *scheduleRestartHarness, events *eventCollector) int64 {
	t.Helper()
	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "initialize", "fake-model", managerAttrs("telegram-main"))
	require.NoError(t, err)
	events.waitMessage(parentID, "ready for schedule")
	return parentID
}

func addFlakyDueOneShot(t *testing.T, h *scheduleRestartHarness, sessionID int64) *failFirstRemoveScheduleStore {
	t.Helper()
	due := time.Now().Add(-time.Minute).UTC()
	_, err := h.schedules.AddSchedule(h.ctx, sessionID, "", &due, "scheduled once", false)
	require.NoError(t, err)
	return &failFirstRemoveScheduleStore{Store: h.schedules, attempted: make(chan struct{})}
}

func waitForScheduledInput(t *testing.T, events *eventCollector, sessionID int64) {
	t.Helper()
	events.waitFor(t, "scheduler input publication", func(got []controllerapi.SessionNotification) bool {
		for _, event := range got {
			if event.SessionID == sessionID && event.Notification.Type == sessionevent.NotifyInputReceived &&
				event.Notification.Source == "scheduler" && event.Notification.Message == "scheduled once" {
				return true
			}
		}
		return false
	})
}

func requireOneShotRemainsRetryable(t *testing.T, h *scheduleRestartHarness, sessionID int64) {
	t.Helper()
	remaining, err := h.schedules.ListSchedules(h.ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, remaining, 1, "failed acknowledgement must leave the accepted one-shot retryable")
	assert.Equal(t, 1, countToolResultsFor(h.messages(sessionID), tool.IDSchedule))
}

func retryOneShotAfterRestart(
	t *testing.T,
	h *scheduleRestartHarness,
	sessionID int64,
) *eventCollector {
	t.Helper()
	// Subscribe before recovery starts to retain its session-created announcement.
	events := collectEvents(t, h.mgr.bus.SubscribeManager("telegram-main"))
	t.Cleanup(events.stop)
	require.NoError(t, h.mgr.Start(h.ctx))
	executor := schedule.NewExecutor(h.schedules, h.mgr)
	executor.Start(h.ctx)
	t.Cleanup(executor.Stop)
	h.waitUntil("restart retry acknowledged", func() bool {
		schedules, err := h.schedules.ListSchedules(h.ctx, sessionID)
		return err == nil && len(schedules) == 0 && !h.mgr.HasActiveLoop(sessionID)
	})
	executor.Stop()
	return events
}

func assertNoRetryPublication(t *testing.T, events []controllerapi.SessionNotification) {
	t.Helper()
	for _, event := range events {
		assert.NotEqual(t, sessionevent.NotifyInputReceived, event.Notification.Type,
			"duplicate scheduled delivery must not republish accepted input after restart")
		assert.NotEqual(t, sessionevent.NotifyMessage, event.Notification.Type,
			"duplicate scheduled delivery must not render another answer after restart")
	}
}

func newScheduleRunningObserver(t *testing.T, source sessionbus.Source) *scheduleRunningObserver {
	t.Helper()
	o := &scheduleRunningObserver{
		source: source, subscription: source.SubscribeManager("telegram-main"),
		running: make(chan struct{}), done: make(chan struct{}), stopped: make(chan struct{}),
	}
	go o.watch()
	t.Cleanup(o.close)
	return o
}

func (o *scheduleRunningObserver) watch() {
	defer close(o.stopped)
	for {
		select {
		case <-o.done:
			return
		case event := <-o.subscription:
			if event.Notification.Type == sessionevent.NotifyStateChanged &&
				event.Notification.Status == controllerapi.StateRunning {
				o.closeOnce.Do(func() { close(o.running) })
			}
		}
	}
}

func (o *scheduleRunningObserver) close() {
	o.closeOnce.Do(func() { close(o.running) })
	close(o.done)
	o.source.UnsubscribeManager(o.subscription)
	<-o.stopped
}

func (s *orderedScheduleSender) NotifySession(sessionID int64, n sessionevent.Notification) {
	if n.Type == sessionevent.NotifyInputReceived && n.Source == "scheduler" {
		select {
		case <-s.running:
		case <-s.done:
			return
		}
	}
	s.SessionSender.NotifySession(sessionID, n)
}

func newScheduleRestartHarness(
	t *testing.T,
	dbPath, workDir string,
	respond func(string, []llmwire.Message) *llmwire.Response,
) *scheduleRestartHarness {
	t.Helper()
	base := newHarness(
		t,
		harnessOptions{dbPath: dbPath, respond: respond, configure: func(cfg *config.Config) { cfg.WorkDir = workDir }},
	)
	h := &scheduleRestartHarness{harness: base, db: base.db}
	t.Cleanup(func() { require.NoError(t, h.close()) })
	return h
}

func (h *scheduleRestartHarness) close() error {
	h.closeOnce.Do(func() {
		h.shutdown()
		h.closeErr = h.db.Close()
	})
	return h.closeErr
}

func countMessageContentContaining(messages []llmwire.Message, fragment string) int {
	count := 0
	for _, message := range messages {
		if strings.Contains(message.Content, fragment) {
			count++
		}
	}
	return count
}

type stoppedRootScheduleCase struct {
	name, prompt, answer, trace string
	fresh                       bool
}

func runStoppedRootScheduleScenario(t *testing.T, tc stoppedRootScheduleCase) {
	t.Helper()
	started := make(chan struct{})
	release := make(chan struct{})
	h := newHarness(t, harnessOptions{respond: stoppedRootScheduleResponder(tc, started, release)})
	h.startInboxWake()
	rootID, err := h.mgr.Send(t.Context(), h.projectID, "initialize", "fake-model", managerAttrs("telegram-main"))
	require.NoError(t, err)
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(rootID) })
	require.NoError(t, h.mgr.sendToSession(t.Context(), rootID, "/stop"))
	collector := collectEvents(t, h.mgr.bus.SubscribeManager("telegram-main"))
	t.Cleanup(collector.stop)
	oldEpisode := time.Now().UTC().Add(-time.Hour)
	_, err = h.db.ExecContext(t.Context(),
		`UPDATE sessions SET episode_started_at = ? WHERE id = ?`, oldEpisode, rootID)
	require.NoError(t, err)
	deliveryID := runDueStoppedRootSchedule(t, h, rootID, collector, tc, started, release)
	successfulTrace := collector.snapshot()
	newEpisode := sessionEpisodeStart(t, h, rootID)
	assert.True(t, newEpisode.After(oldEpisode))
	assertStoppedRootScheduleResult(t, h, rootID, tc)
	assertStoppedRootScheduleDuplicate(t, h, rootID, deliveryID, tc, newEpisode)
	assertHarnessTrace(t, tc.trace, successfulTrace, rootID)
}

func scheduledTurnRequested(tc stoppedRootScheduleCase, messages []llmwire.Message) bool {
	if tc.fresh {
		return hasUserContaining(messages, tc.prompt)
	}
	return hasToolResultFor(messages, tool.IDSchedule)
}

func runDueStoppedRootSchedule(
	t *testing.T,
	h *harness,
	rootID int64,
	collector *eventCollector,
	tc stoppedRootScheduleCase,
	started <-chan struct{},
	release chan<- struct{},
) string {
	t.Helper()
	due := time.Now().Add(-time.Minute).UTC()
	entry, err := h.schedules.AddSchedule(t.Context(), rootID, "", &due, tc.prompt, tc.fresh)
	require.NoError(t, err)
	observer := newScheduleRunningObserver(t, h.mgr.bus)
	sender := &orderedScheduleSender{SessionSender: h.mgr, running: observer.running, done: observer.done}
	executor := schedule.NewExecutor(h.schedules, sender)
	executor.Start(t.Context())
	t.Cleanup(executor.Stop)
	requireSignal(t, started)
	assertStoppedRootActive(t, h, rootID)
	close(release)
	h.waitUntil("runDueStoppedRootSchedule", func() bool {
		entries, listErr := h.schedules.ListSchedules(t.Context(), rootID)
		return listErr == nil && len(entries) == 0 && !h.mgr.HasActiveLoop(rootID) &&
			lastAssistantTextDTO(h.messages(rootID)) == tc.answer
	})
	executor.Stop()
	collector.waitMessage(rootID, tc.answer)
	return fmt.Sprintf("schedule:one-shot:%d", entry.ID())
}

func assertStoppedRootActive(t *testing.T, h *harness, rootID int64) {
	t.Helper()
	rec := h.session(rootID)
	assert.Equal(t, sessionstore.SessionStatusActive, rec.Status)
}

func assertStoppedRootScheduleResult(t *testing.T, h *harness, rootID int64, tc stoppedRootScheduleCase) {
	t.Helper()
	messages := h.messages(rootID)
	if tc.fresh {
		// The confirmed stop publishes one answer; its hidden candidate row stays in the transcript with the same text.
		prompt := tc.prompt
		promptCount := 0
		for _, message := range messages {
			if strings.Contains(message.Content, prompt) {
				promptCount++
			}
		}
		assert.Equal(t, 1, promptCount)
		answer := tc.answer
		answerCount := 0
		for _, message := range messages {
			if strings.Contains(message.Content, answer) {
				answerCount++
			}
		}
		assert.Equal(t, 2, answerCount)
		return
	}
	assert.Equal(t, 1, countToolResultsFor(messages, tool.IDSchedule))
}

func assertStoppedRootScheduleDuplicate(
	t *testing.T,
	h *harness,
	rootID int64,
	deliveryID string,
	tc stoppedRootScheduleCase,
	episodeStartedAt time.Time,
) {
	t.Helper()
	require.NoError(t, h.mgr.sendToSession(t.Context(), rootID, "/stop"))
	applied, err := deliverStoppedRootSchedule(t, h.mgr, rootID, deliveryID, tc)
	require.NoError(t, err)
	assert.False(t, applied, "an acknowledged retry must not create another turn")
	h.waitUntil("duplicate acknowledged", func() bool { return !h.mgr.HasActiveLoop(rootID) })
	rec := h.session(rootID)
	assert.Equal(t, sessionstore.SessionStatusStopped, rec.Status)
	assert.Equal(t, episodeStartedAt, sessionEpisodeStart(t, h, rootID))
	assertStoppedRootScheduleResult(t, h, rootID, tc)
	_, err = enqueueCallResult(t.Context(), h.mgr.store, rootID, "missing-call", tool.IDSleep, "must stay stopped")
	require.NoError(t, err)
	assert.False(t, h.mgr.HasActiveLoop(rootID), "a late call result must not revive a stopped root")
	record := h.session(rootID)
	assert.Equal(t, sessionstore.SessionStatusStopped, record.Status)
}

func sessionEpisodeStart(t *testing.T, h *harness, rootID int64) time.Time {
	t.Helper()
	var startedAt time.Time
	require.NoError(t, h.db.QueryRowContext(t.Context(),
		`SELECT episode_started_at FROM sessions WHERE id = ?`, rootID).Scan(&startedAt))
	return startedAt
}

func deliverStoppedRootSchedule(
	t *testing.T,
	mgr *svc,
	rootID int64,
	deliveryID string,
	tc stoppedRootScheduleCase,
) (bool, error) {
	t.Helper()
	if tc.fresh {
		return enqueueScheduledInput(t.Context(), mgr.store, rootID, deliveryID, tc.prompt, true)
	}
	return enqueueScheduledInput(t.Context(), mgr.store, rootID, deliveryID, tc.prompt, false)
}

func createScheduleBoundarySubagent(t *testing.T, h *harness) int64 {
	t.Helper()
	parent := h.createRoot(nil)
	childID := h.createUnlinkedChild(parent)
	require.NoError(t, h.store.UpdateSessionStatus(t.Context(), childID, sessionstore.SessionStatusCompleted))
	return childID
}

func scheduleRestartResponder(release <-chan struct{}) func(string, []llmwire.Message) *llmwire.Response {
	return func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasToolResultFor(messages, tool.IDSchedule) {
			<-release
			return textReply("scheduled work completed")
		}
		return textReply("ready for schedule")
	}
}

func stoppedRootScheduleResponder(
	tc stoppedRootScheduleCase,
	started chan<- struct{},
	release <-chan struct{},
) func(string, []llmwire.Message) *llmwire.Response {
	// The completion check calls the model twice for one scheduled turn; only the first call arms the signal.
	var once sync.Once
	return func(_ string, messages []llmwire.Message) *llmwire.Response {
		if scheduledTurnRequested(tc, messages) {
			once.Do(func() { close(started) })
			<-release
			return textReply(tc.answer)
		}
		return textReply("ready")
	}
}

func enqueueScheduledInput(ctx context.Context, store Store, id int64, key, content string, fresh bool) (bool, error) {
	result, err := store.Enqueue(
		ctx, sessionstore.Input{
			SessionID:   id,
			Source:      sessionstore.InputSourceSchedule,
			Content:     content,
			DeliveryKey: key,
			Attributes:  map[string]any{"fresh": fresh},
		},
	)
	if err != nil {
		return false, err
	}
	return result.Applied, nil
}
