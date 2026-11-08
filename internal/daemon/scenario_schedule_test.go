package daemon

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llm/llmtest"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/sessionbus"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
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
	due := time.Now().UTC()
	rootSchedule, err := h.schedules.AddSchedule(
		t.Context(),
		rootID,
		"",
		&due,
		"scheduled task",
		false,
		schedule.ScheduleMetadata{},
	)
	require.NoError(t, err)
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
		actual := applyScheduleBoundaryCommand(t, h, rootID, subagentID, rootSchedule, command)
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
	parentID, events := deliverOneShotBeforeRestart(t, first)
	require.NoError(t, first.close(), "the first daemon must close SQLite before restart")
	second := newScheduleRestartHarness(t, dbPath, workDir, respond)
	retryEvents := retryOneShotAfterRestart(t, second, parentID, releaseScheduledRun)
	retryEvents.waitMessage(parentID, "scheduled work completed")
	retryEvents.waitIdleAfter(parentID, "scheduled work completed")
	assert.Equal(t, 1, countToolResultsFor(second.messages(parentID), tool.IDSchedule))
	assert.Equal(t, "scheduled work completed", lastAssistantTextDTO(second.messages(parentID)))
	trace := append(events.snapshot(), retryEvents.snapshot()...)
	// Restart announcements race runner drains; pin message and delivery order instead of activation timing noise.
	assertHarnessTrace(t, second.harness, "one_shot_ack_retry_restart.json", dropTransientEvents(trace), parentID)
	require.NoError(t, second.close())
	third := newScheduleRestartHarness(t, dbPath, workDir, respond)
	assertNoRetryPublication(t, retryOneShotAfterRestart(t, third, parentID, nil).snapshot())
	assert.Equal(t, 1, countToolResultsFor(third.messages(parentID), tool.IDSchedule))
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
	turn := collector.waitIdleAfter(parentID, "sleep interruption handled")
	messages := h.messages(parentID)
	require.NoError(t, llmtest.ValidateToolPairing(messages))
	assert.Equal(t, 1, countToolResultsFor(messages, tool.IDSleep))
	assert.Contains(t, scheduleLastToolResultContent(messages, tool.IDSleep), "Sleep interrupted")
	schedules, err := h.schedules.ListSchedules(h.ctx, parentID)
	require.NoError(t, err)
	assert.Empty(t, schedules)
	assertHarnessTrace(t, h, "sleep_interrupted_by_input.json", turn, parentID)
}

// Without the wake loop the runner is the only caller that can observe the stale sleep row.
func TestHarnessScenario_SleepInterruptedWhileRowRetiredConcurrently(t *testing.T) {
	const answer = "sleep interruption handled"
	t.Run("root trace", func(t *testing.T) {
		h, retired := newStaleSleepHarness(t, sleepThenAnswer(answer))
		collector := collectEvents(t, h.mgr.bus.SubscribeAll())
		defer collector.stop()
		id, turn := interruptSleep(t, h, collector, nil, answer)
		assert.EqualValues(t, 1, retired.retired.Load())
		assert.Equal(t, sessionstore.SessionStatusCompleted, h.session(id).Status)
		assertHarnessTraceForScenario(t, h, "TestHarnessScenario_SleepProjectsWakeAtAndUserInputInterruptsIt",
			"sleep_interrupted_by_input.json", turn, id)
	})
	t.Run("manager root", func(t *testing.T) {
		h, retired := newStaleSleepHarness(t, sleepThenAnswer(answer))
		collector := collectEvents(t, h.mgr.bus.SubscribeAll())
		defer collector.stop()
		id, _ := interruptSleep(t, h, collector, managerAttrs(scenarioManagerID), answer)
		assert.EqualValues(t, 1, retired.retired.Load())
		assert.Equal(t, sessionstore.SessionStatusCompleted, h.session(id).Status)
		outputs, _ := scenarioOutputs(t, h)
		contents := make([]string, 0, len(outputs))
		for _, output := range outputs {
			contents = append(contents, output.Content)
		}
		assert.Equal(t, []string{"", "→ sleep", answer}, contents)
	})
	t.Run("subagent", func(t *testing.T) {
		runInterruptedSubagentScenario(t)
	})
}

func runInterruptedSubagentScenario(t *testing.T) {
	t.Helper()
	const answer = "child answer after interruption"
	h, retired := newStaleSleepHarness(t, func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasUserContaining(messages, "CHILD_TASK") {
			return sleepThenAnswer(answer)("", messages)
		}
		if hasToolResultFor(messages, tool.IDTask) {
			return textReply("parent ack")
		}
		return callReply(taskCallID, tool.IDTask,
			`{"prompt":"CHILD_TASK","description":"child work","subagent_type":"general","background":true}`)
	})
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "delegate", "fake-model", nil)
	require.NoError(t, err)
	var childID int64
	h.waitUntil("child asleep", func() bool {
		links, listErr := h.links.ListPendingChildLinks(h.ctx, parentID)
		if listErr != nil || len(links) != 1 {
			return false
		}
		childID = links[0].ChildID
		entries, scheduleErr := h.schedules.ListSchedules(h.ctx, childID)
		return scheduleErr == nil && len(entries) == 1 && !h.mgr.HasActiveLoop(childID)
	})
	require.NoError(t, h.mgr.SendToChild(h.ctx, childID, "wake up early"))
	var link *subagent.Link
	h.waitUntil("child terminal", func() bool {
		var linkErr error
		link, linkErr = h.links.GetLink(h.ctx, childID)
		return linkErr == nil && link != nil && link.Terminal()
	})
	assert.EqualValues(t, 1, retired.retired.Load())
	assert.Equal(t, subagent.StateCompleted, link.State)
	assert.Equal(t, answer, link.Result)
}

// A wake that read the input before its acceptance may start one runner after the turn ended.
func TestHarnessScenario_LateWakeAfterInterruptedSleepAddsNoOutput(t *testing.T) {
	const answer = "sleep interruption handled"
	var modelCalls atomic.Int64
	respond := sleepThenAnswer(answer)
	h := newHarness(t, harnessOptions{respond: func(model string, messages []llmwire.Message) *llmwire.Response {
		modelCalls.Add(1)
		return respond(model, messages)
	}})
	collector := collectEvents(t, h.mgr.bus.SubscribeAll())
	defer collector.stop()
	id, _ := interruptSleep(t, h, collector, managerAttrs(scenarioManagerID), answer)
	h.waitUntil("turn runner gone", func() bool { return !h.mgr.HasActiveLoop(id) })
	before := collector.snapshot()
	outputs, owners := scenarioOutputs(t, h)
	messages, status, calls := h.messages(id), h.session(id).Status, modelCalls.Load()

	require.NoError(t, h.mgr.start(h.ctx, id))
	collector.waitFor(t, "late activation idle", func(events []controllerapi.SessionNotification) bool {
		return hasStateEvent(events[len(before):], id, sessionevent.StateIdle)
	})
	h.waitUntil("late runner gone", func() bool { return !h.mgr.HasActiveLoop(id) })

	lateEvents := 0
	for _, event := range collector.snapshot()[len(before):] {
		if event.SessionID != id {
			continue
		}
		lateEvents++
		assert.Contains(t, []sessionevent.NotificationType{
			sessionevent.NotifySessionCreated, sessionevent.NotifyStateChanged,
		}, event.Notification.Type)
	}
	require.Positive(t, lateEvents)
	lateOutputs, lateOwners := scenarioOutputs(t, h)
	assert.Equal(t, outputs, lateOutputs)
	assert.Equal(t, owners, lateOwners)
	assert.Equal(t, messages, h.messages(id))
	assert.Equal(t, status, h.session(id).Status)
	assert.Equal(t, calls, modelCalls.Load())
}

// Another owner retires the row after the caller listed it and before the caller deletes it.
type staleSleepRetiredFirstStore struct {
	schedule.Store
	retired atomic.Int64
}

func (s *staleSleepRetiredFirstStore) CallPending(ctx context.Context, sessionID int64, callID string) (bool, error) {
	pending, err := s.Store.CallPending(ctx, sessionID, callID)
	if err != nil || pending {
		return pending, err
	}
	removed, err := s.RemoveSleepSchedules(ctx, sessionID)
	s.retired.Add(removed)
	return false, err
}

func newStaleSleepHarness(
	t *testing.T,
	respond func(string, []llmwire.Message) *llmwire.Response,
) (*harness, *staleSleepRetiredFirstStore) {
	t.Helper()
	retired := &staleSleepRetiredFirstStore{}
	h := newHarness(t, harnessOptions{respond: respond, schedules: func(store schedule.Store) schedule.Store {
		retired.Store = store
		return retired
	}})
	return h, retired
}

func sleepThenAnswer(answer string) func(string, []llmwire.Message) *llmwire.Response {
	return func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasToolResultFor(messages, tool.IDSleep) {
			return textReply(answer)
		}
		return callReply("scenario-sleep", tool.IDSleep, `{"duration":"1h","reason":"scenario"}`)
	}
}

func interruptSleep(
	t *testing.T,
	h *harness,
	collector *eventCollector,
	attrs map[string]any,
	answer string,
) (int64, []controllerapi.SessionNotification) {
	t.Helper()
	id, err := h.mgr.Send(h.ctx, h.projectID, "sleep", "fake-model", attrs)
	require.NoError(t, err)
	collector.waitWait(id, sessionevent.WaitSleep)
	h.waitUntil("sleeping runner gone", func() bool { return !h.mgr.HasActiveLoop(id) })
	require.NoError(t, h.mgr.sendToSession(h.ctx, id, "interrupt now"))
	collector.waitMessage(id, answer)
	return id, collector.waitIdleAfter(id, answer)
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
	require.NoError(t, llmtest.ValidateToolPairing(msgs))
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
	_, err = h.schedules.AddSchedule(
		h.ctx,
		parentID,
		"",
		&standaloneAt,
		"standalone future input",
		false,
		schedule.ScheduleMetadata{},
	)
	require.NoError(t, err)
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, parentID, "interrupt now"))
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(parentID) })
	messages := h.messages(parentID)
	require.NoError(t, llmtest.ValidateToolPairing(messages))
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
	_, err = h.schedules.AddSchedule(
		h.ctx,
		parentID,
		"",
		&due,
		"one-time scheduled work",
		false,
		schedule.ScheduleMetadata{},
	)
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
	require.NoError(t, llmtest.ValidateToolPairing(messages))
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
	_, err = h.schedules.AddSchedule(
		h.ctx,
		parentID,
		"",
		&due,
		"one-time retry-safe work",
		false,
		schedule.ScheduleMetadata{},
	)
	require.NoError(t, err)
	sub := h.mgr.bus.Subscribe(parentID)
	defer h.mgr.bus.Unsubscribe(parentID, sub)
	flaky := failNextScheduleDelivery(t, h)
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
	assert.Zero(t, countToolResultsFor(h.messages(parentID), tool.IDSchedule))
	second := schedule.NewExecutor(h.schedules, h.mgr)
	second.Start(h.ctx)
	h.waitUntil("one-shot retry acknowledged", func() bool {
		schedules, listErr := h.schedules.ListSchedules(h.ctx, parentID)
		return listErr == nil && len(schedules) == 0 && !h.mgr.HasActiveLoop(parentID) &&
			lastAssistantTextDTO(h.messages(parentID)) == "one-shot handled once"
	})
	second.Stop()
	messages := h.messages(parentID)
	require.NoError(t, llmtest.ValidateToolPairing(messages))
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
	entry, err := h.schedules.AddSchedule(
		h.ctx,
		parentID,
		"* * * * *",
		nil,
		"fresh scheduled work",
		true,
		schedule.ScheduleMetadata{},
	)
	require.NoError(t, err)
	minute := time.Now().UTC().Truncate(time.Minute)
	applied, err := h.schedules.DeliverCron(
		h.ctx,
		entry,
		entry.InputMessage(),
		map[string]any{"fresh": true},
		minute,
		minute,
	)
	require.NoError(t, err)
	assert.True(t, applied)
	h.waitUntil("fresh schedule completed", func() bool {
		return countMessageContentContaining(h.messages(parentID), "fresh handled once") == 2
	})
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(parentID) })
	applied, err = h.schedules.DeliverCron(
		h.ctx,
		entry,
		entry.InputMessage(),
		map[string]any{"fresh": true},
		minute,
		minute.Add(40*time.Second),
	)
	require.NoError(t, err)
	assert.False(t, applied)
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(parentID) })
	messages := h.messages(parentID)
	require.NoError(t, llmtest.ValidateToolPairing(messages))
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
		deliver func(schedule.Store, int64) (bool, error)
	}{
		{
			name: "normal",
			deliver: func(store schedule.Store, sessionID int64) (bool, error) {
				return enqueueScheduledInput(
					t.Context(), store, sessionID, "legacy task", false,
				)
			},
		},
		{
			name: "fresh",
			deliver: func(store schedule.Store, sessionID int64) (bool, error) {
				return enqueueScheduledInput(
					t.Context(), store, sessionID, "legacy fresh task", true,
				)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, harnessOptions{respond: trivialRespond})
			childID := createScheduleBoundarySubagent(t, h)
			before := h.messages(childID)
			applied, err := tt.deliver(h.schedules, childID)
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
	_, err := h.schedules.AddSchedule(t.Context(), childID, "", &due, "legacy task", false, schedule.ScheduleMetadata{})
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
			entry, err := h.schedules.AddSchedule(
				t.Context(),
				childID,
				"* * * * *",
				nil,
				"legacy task",
				tt.fresh,
				schedule.ScheduleMetadata{},
			)
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
		h.ctx, h.schedules, root, "produce the weekly report", false,
	)
	require.NoError(t, err)
	require.True(t, delivered)
	collector.waitMessage(root, "Report ready.")
	collector.waitIdleAfter(root, "Report ready.")
	assertHarnessTrace(t, h, "scheduled_turn_chain.json", collector.snapshot(), root)
}

func scheduleLastToolResultContent(msgs []llmwire.Message, toolName string) string {
	for _, v := range slices.Backward(msgs) {
		if v.Role == llmwire.RoleTool && v.ToolName == toolName {
			return v.Content
		}
	}
	return ""
}

type failFirstScheduleDeliveryStore struct {
	schedule.Store
	db        *sql.DB
	once      sync.Once
	attempted chan struct{}
}

func failNextScheduleDelivery(t *testing.T, h *harness) *failFirstScheduleDeliveryStore {
	t.Helper()
	_, err := h.db.ExecContext(t.Context(), `CREATE TRIGGER fail_schedule_delivery BEFORE INSERT ON session_inbox
		WHEN NEW.source = 'schedule' BEGIN SELECT RAISE(ABORT, 'forced enqueue failure'); END`)
	require.NoError(t, err)
	return &failFirstScheduleDeliveryStore{Store: h.schedules, db: h.db, attempted: make(chan struct{})}
}

func (s *failFirstScheduleDeliveryStore) DeliverOneShot(
	ctx context.Context,
	sched *schedule.Schedule,
	content string,
	attrs map[string]any,
) (bool, error) {
	first := false
	s.once.Do(func() { first = true })
	applied, err := s.Store.DeliverOneShot(ctx, sched, content, attrs)
	if !first {
		return applied, err
	}
	_, dropErr := s.db.ExecContext(ctx, `DROP TRIGGER fail_schedule_delivery`)
	close(s.attempted)
	return applied, errors.Join(err, dropErr)
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
	rootSchedule *schedule.Schedule,
	command scheduleBoundaryCommand,
) scheduleBoundaryObservation {
	t.Helper()
	applied, err := executeScheduleBoundaryCommand(t, h, rootID, subagentID, rootSchedule, command)
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
	rootSchedule *schedule.Schedule,
	command scheduleBoundaryCommand,
) (bool, error) {
	t.Helper()
	switch command {
	case deliverSubagentTick:
		return enqueueScheduledInput(
			t.Context(), h.schedules, subagentID, "legacy task", false,
		)
	case deliverSubagentFresh:
		return enqueueScheduledInput(
			t.Context(), h.schedules, subagentID, "legacy fresh task", true,
		)
	case deliverStoppedRoot, deliverDuplicateRoot:
		return h.schedules.DeliverOneShot(t.Context(), rootSchedule, "scheduled task", map[string]any{"fresh": false})
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
	source       *sessionbus.Bus
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
) (int64, *eventCollector) {
	t.Helper()
	events := collectEvents(t, h.mgr.bus.SubscribeManager("telegram-main"))
	t.Cleanup(events.stop)
	parentID := createScheduleSession(t, h, events)
	flaky := addFlakyDueOneShot(t, h, parentID)
	executor := schedule.NewExecutor(flaky, h.mgr)
	executor.Start(h.ctx)
	t.Cleanup(executor.Stop)
	requireSignal(t, flaky.attempted)
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

func addFlakyDueOneShot(t *testing.T, h *scheduleRestartHarness, sessionID int64) *failFirstScheduleDeliveryStore {
	t.Helper()
	due := time.Now().Add(-time.Minute).UTC()
	_, err := h.schedules.AddSchedule(h.ctx, sessionID, "", &due, "scheduled once", false, schedule.ScheduleMetadata{})
	require.NoError(t, err)
	return failNextScheduleDelivery(t, h.harness)
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
	require.Len(t, remaining, 1, "failed transaction must leave the one-shot retryable")
	assert.Zero(t, countToolResultsFor(h.messages(sessionID), tool.IDSchedule))
}

func retryOneShotAfterRestart(
	t *testing.T,
	h *scheduleRestartHarness,
	sessionID int64,
	release chan<- struct{},
) *eventCollector {
	t.Helper()
	// Subscribe before recovery starts to retain its session-created announcement.
	events := collectEvents(t, h.mgr.bus.SubscribeManager("telegram-main"))
	t.Cleanup(events.stop)
	require.NoError(t, h.mgr.Start(h.ctx))
	observer := newScheduleRunningObserver(t, h.mgr.bus)
	sender := &orderedScheduleSender{SessionSender: h.mgr, running: observer.running, done: observer.done}
	executor := schedule.NewExecutor(h.schedules, sender)
	executor.Start(h.ctx)
	t.Cleanup(executor.Stop)
	if release != nil {
		waitForScheduledInput(t, events, sessionID)
		close(release)
	}
	h.waitUntil("restart retry acknowledged", func() bool {
		schedules, err := h.schedules.ListSchedules(h.ctx, sessionID)
		return err == nil && len(schedules) == 0 && !h.mgr.HasActiveLoop(sessionID) &&
			lastAssistantTextDTO(h.messages(sessionID)) == "scheduled work completed"
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

func newScheduleRunningObserver(t *testing.T, source *sessionbus.Bus) *scheduleRunningObserver {
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
	entry := runDueStoppedRootSchedule(t, h, rootID, collector, tc, started, release)
	successfulTrace := collector.snapshot()
	newEpisode := sessionEpisodeStart(t, h, rootID)
	assert.True(t, newEpisode.After(oldEpisode))
	assertStoppedRootScheduleResult(t, h, rootID, tc)
	assertStoppedRootScheduleDuplicate(t, h, rootID, entry, tc, newEpisode)
	assertHarnessTrace(t, h, tc.trace, successfulTrace, rootID)
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
) *schedule.Schedule {
	t.Helper()
	due := time.Now().Add(-time.Minute).UTC()
	entry, err := h.schedules.AddSchedule(
		t.Context(),
		rootID,
		"",
		&due,
		tc.prompt,
		tc.fresh,
		schedule.ScheduleMetadata{},
	)
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
	return entry
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
	entry *schedule.Schedule,
	tc stoppedRootScheduleCase,
	episodeStartedAt time.Time,
) {
	t.Helper()
	require.NoError(t, h.mgr.sendToSession(t.Context(), rootID, "/stop"))
	applied, err := h.schedules.DeliverOneShot(t.Context(), entry, tc.prompt, map[string]any{"fresh": tc.fresh})
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

func enqueueScheduledInput(
	ctx context.Context,
	store schedule.Store,
	id int64,
	content string,
	fresh bool,
) (bool, error) {
	due := time.Now().UTC()
	entry, err := store.AddSchedule(ctx, id, "", &due, content, fresh, schedule.ScheduleMetadata{})
	if err != nil {
		return false, err
	}
	return store.DeliverOneShot(ctx, entry, content, map[string]any{"fresh": fresh})
}
