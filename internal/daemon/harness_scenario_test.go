package daemon

import (
	"fmt"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/tool"
)

func TestHarnessScenario_SecondInputDoesNotReplayPreviousFinal(t *testing.T) {
	h := newSubagentHarnessWith(t, func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasUserContaining(messages, "second question") {
			return &llmwire.Response{Text: "second answer"}
		}

		return &llmwire.Response{Text: "first answer"}
	})
	collector := collectEvents(h.mgr.PubSub().SubscribeAll())
	defer func() {
		collector.stop()
		h.shutdown()
	}()

	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "first question", "fake-model", nil)
	require.NoError(t, err)
	waitForVisibleMessage(t, collector, sessionID, "first answer")
	waitForIdleAfterMessage(t, collector, sessionID, "first answer")

	require.NoError(t, h.mgr.SendToSession(h.ctx, sessionID, "second question"))
	waitForVisibleMessage(t, collector, sessionID, "second answer")
	waitForIdleAfterMessage(t, collector, sessionID, "second answer")

	assertHarnessTrace(t, "second_input_no_replay.json", collector.snapshot(), sessionID)
}

func TestHarnessScenario_CLIConversationIsManagerOwned(t *testing.T) {
	h := newSubagentHarnessWith(t, func(string, []llmwire.Message) *llmwire.Response {
		return &llmwire.Response{Text: "configuration answer"}
	})
	collector := collectEvents(h.mgr.PubSub().SubscribeAll())
	defer func() {
		collector.stop()
		h.shutdown()
	}()

	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "configure coagent", "fake-model", map[string]any{
		controllerapi.SessionAttributeManagerID: "cli",
		"channel":                               "cli",
	})
	require.NoError(t, err)
	waitForVisibleMessage(t, collector, sessionID, "configuration answer")

	assertHarnessTrace(t, "cli_conversation_manager_owned.json", collector.snapshot(), sessionID)
}

func TestHarnessScenario_ForegroundChildContinuesWithoutSleep(t *testing.T) {
	initialRelease := make(chan struct{})
	followUpRelease := make(chan struct{})
	var childID atomic.Int64

	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasUserContaining(messages, "CHILD_INITIAL") {
			if hasUserContaining(messages, "FOLLOW_UP") {
				<-followUpRelease
				return &llmwire.Response{Text: "child continuation answer"}
			}

			// Hold the child until the parent's suspension has projected its
			// "waiting: subagent" state — the exact event this scenario requires. An
			// instantly terminal child would race publishWaiting and legitimately
			// project nothing.
			<-initialRelease

			return &llmwire.Response{Text: "child initial answer"}
		}

		if hasToolResultFor(messages, "subagent_event") {
			return &llmwire.Response{Text: "continuation delivered"}
		}

		if hasUserContaining(messages, "continue the same child") {
			if hasToolResultFor(messages, "send_to_subagent") {
				return &llmwire.Response{Text: "follow-up accepted"}
			}

			id := childID.Load()
			if id == 0 {
				panic("scenario asked for follow-up before child id was captured")
			}

			return &llmwire.Response{ToolCalls: []llmwire.ToolCall{
				{
					ID:   "follow-up-call",
					Name: tool.IDSendToSubagent,
					Arguments: fmt.Appendf(nil,
						`{"id":%d,"message":"FOLLOW_UP inspect one more thing"}`,
						id,
					),
				},
				{
					ID:        "competing-sleep",
					Name:      tool.IDSleep,
					Arguments: []byte(`{"duration":"1h","reason":"wait for follow-up"}`),
				},
			}}
		}

		if hasToolResultFor(messages, "task") {
			return &llmwire.Response{Text: "initial child delivered"}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID:   taskCallID,
			Name: "task",
			Arguments: []byte(
				`{"prompt":"CHILD_INITIAL","description":"scenario","subagent_type":"general"}`,
			),
		}}}
	}

	h := newSubagentHarnessWith(t, respond)
	collector := collectEvents(h.mgr.PubSub().SubscribeAll())
	defer func() {
		closeOnce(initialRelease)
		closeOnce(followUpRelease)
		collector.stop()
		h.shutdown()
	}()

	parentID, err := h.mgr.Send(h.ctx, h.projectID, "start foreground child", "fake-model", nil)
	require.NoError(t, err)
	waitForWaitKind(t, collector, parentID, sessionevent.WaitSubagent)
	close(initialRelease)
	waitForVisibleMessage(t, collector, parentID, "initial child delivered")
	waitForIdleAfterMessage(t, collector, parentID, "initial child delivered")

	link, err := h.links.GetLinkByTaskCallID(h.ctx, parentID, taskCallID)
	require.NoError(t, err)
	require.NotNil(t, link)
	require.True(t, link.Blocking, "the initial task must exercise foreground mode")
	childID.Store(link.ChildID)

	require.NoError(t, h.mgr.SendToSession(h.ctx, parentID, "continue the same child"))
	waitForVisibleMessage(t, collector, parentID, "follow-up accepted")
	waitForIdleAfterMessage(t, collector, parentID, "follow-up accepted")

	close(followUpRelease)
	waitForVisibleMessage(t, collector, parentID, "continuation delivered")
	waitForIdleAfterMessage(t, collector, parentID, "continuation delivered")

	continued, err := h.links.GetLink(h.ctx, link.ChildID)
	require.NoError(t, err)
	require.NotNil(t, continued)
	assert.Equal(t, int64(2), continued.ActivationSeq)
	assert.False(t, continued.Blocking, "a foreground child continues asynchronously after its task result")
	parentMessages := h.parentMessages(parentID)
	assert.Equal(t, 1, countToolResultsFor(parentMessages, tool.IDSleep))
	assert.Contains(t, lastToolResultContent(parentMessages, tool.IDSleep),
		"sleep cannot be combined with send_to_subagent")
	schedules, err := h.schedStore.ListSchedules(h.ctx, parentID)
	require.NoError(t, err)
	assert.Empty(t, schedules, "rejected sleep must not leave a competing wake-up")
	assertHarnessTrace(t, "foreground_followup_no_sleep.json", collector.snapshot(), parentID)
}

func TestHarnessScenario_BackgroundChildIsTheWakeSource(t *testing.T) {
	childRelease := make(chan struct{})

	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasUserContaining(messages, "CHILD_BACKGROUND") {
			<-childRelease

			return &llmwire.Response{Text: "background child answer"}
		}

		if hasToolResultFor(messages, "subagent_event") {
			return &llmwire.Response{Text: "background completion delivered"}
		}

		if hasToolResultFor(messages, tool.IDSleep) {
			return &llmwire.Response{Text: "background launched; yielded without sleep"}
		}

		if hasToolResultFor(messages, tool.IDTask) {
			return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
				ID:        "sleep-after-task-result",
				Name:      tool.IDSleep,
				Arguments: []byte(`{"duration":"1h","reason":"wait for background child"}`),
			}}}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID:   taskCallID,
			Name: tool.IDTask,
			Arguments: []byte(
				`{"prompt":"CHILD_BACKGROUND wait for release","description":"scenario","subagent_type":"general","background":true}`,
			),
		}}}
	}

	h := newSubagentHarnessWith(t, respond)
	collector := collectEvents(h.mgr.PubSub().SubscribeAll())
	defer func() {
		closeOnce(childRelease)
		collector.stop()
		h.shutdown()
	}()

	parentID, err := h.mgr.Send(h.ctx, h.projectID, "start background child", "fake-model", nil)
	require.NoError(t, err)
	waitForVisibleMessage(t, collector, parentID, "background launched; yielded without sleep")
	waitForIdleAfterMessage(t, collector, parentID, "background launched; yielded without sleep")

	link, err := h.links.GetLinkByTaskCallID(h.ctx, parentID, taskCallID)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.False(t, link.Blocking)

	parentMessages := h.parentMessages(parentID)
	assert.Equal(t, 1, countToolResultsFor(parentMessages, tool.IDSleep))
	assert.Contains(t, lastToolResultContent(parentMessages, tool.IDSleep),
		"subagent will wake the session automatically")
	schedules, err := h.schedStore.ListSchedules(h.ctx, parentID)
	require.NoError(t, err)
	assert.Empty(t, schedules, "pending child must remain the sole wake source")

	close(childRelease)
	waitForVisibleMessage(t, collector, parentID, "background completion delivered")
	waitForIdleAfterMessage(t, collector, parentID, "background completion delivered")

	assertHarnessTrace(t, "background_child_no_sleep.json", collector.snapshot(), parentID)
}

func TestHarnessScenario_ForegroundScatterGatherProjectsShrinkingAllWaitSet(t *testing.T) {
	callIDs := []string{"wait-1", "wait-2", "wait-3"}
	releases := []chan struct{}{make(chan struct{}), make(chan struct{}), make(chan struct{})}

	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		for i := range releases {
			if hasUserContaining(messages, fmt.Sprintf("CHILD_WAIT_%d", i+1)) {
				<-releases[i]

				return &llmwire.Response{Text: fmt.Sprintf("child %d done", i+1)}
			}
		}

		if hasAssistantToolCall(messages, tool.IDTask) {
			return &llmwire.Response{Text: "all children delivered"}
		}

		calls := make([]llmwire.ToolCall, len(callIDs))
		for i, callID := range callIDs {
			calls[i] = llmwire.ToolCall{
				ID:   callID,
				Name: tool.IDTask,
				Arguments: fmt.Appendf(nil,
					`{"prompt":"CHILD_WAIT_%d","description":"wait child","subagent_type":"general"}`,
					i+1,
				),
			}
		}

		return &llmwire.Response{ToolCalls: calls}
	}

	h := newSubagentHarnessWith(t, respond)
	collector := collectEvents(h.mgr.PubSub().SubscribeAll())
	defer func() {
		for _, release := range releases {
			closeOnce(release)
		}
		collector.stop()
		h.shutdown()
	}()

	parentID, err := h.mgr.Send(h.ctx, h.projectID, "scatter gather", "fake-model", nil)
	require.NoError(t, err)

	releaseOf := map[int64]chan struct{}{}
	childIDs := make([]int64, 0, len(callIDs))
	for i, callID := range callIDs {
		h.waitUntil("child link for "+callID, func() bool {
			link, linkErr := h.links.GetLinkByTaskCallID(h.ctx, parentID, callID)

			return linkErr == nil && link != nil
		})
		link, linkErr := h.links.GetLinkByTaskCallID(h.ctx, parentID, callID)
		require.NoError(t, linkErr)
		releaseOf[link.ChildID] = releases[i]
		childIDs = append(childIDs, link.ChildID)
	}
	slices.Sort(childIDs)

	waitForSubagentSet(t, collector, parentID, childIDs)
	assert.Zero(t, countPublishedMessage(collector.snapshot(), parentID, "all children delivered"),
		"the model must not run while any foreground child is pending")

	// Release in child-id order: spawn order across concurrent task calls is not
	// fixed, so only this makes the recorded shrink sequence reproducible.
	for i, childID := range childIDs {
		close(releaseOf[childID])
		h.waitForDelivery(childID)

		if i < len(childIDs)-1 {
			waitForSubagentSet(t, collector, parentID, childIDs[i+1:])
		}
	}

	waitForVisibleMessage(t, collector, parentID, "all children delivered")
	waitForIdleAfterMessage(t, collector, parentID, "all children delivered")

	for _, event := range collector.snapshot() {
		if event.SessionID != parentID || event.Notification.Type != sessionevent.NotifyWaiting {
			continue
		}

		for _, item := range event.Notification.Waiting {
			assert.Equal(t, sessionevent.WaitSubagent, item.Kind)
			assert.Nil(t, item.WakeAt)
		}
	}

	assertHarnessTrace(t, "scatter_gather_shrinking_wait_set.json", collector.snapshot(), parentID)
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
	collector := collectEvents(h.mgr.PubSub().SubscribeAll())
	defer func() {
		collector.stop()
		h.shutdown()
	}()

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

	require.NoError(t, h.mgr.SendToSession(h.ctx, parentID, "interrupt now"))
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

func waitForWaitKind(
	t *testing.T,
	collector *eventCollector,
	sessionID int64,
	kind sessionevent.WaitKind,
) {
	t.Helper()
	collector.waitFor(
		t,
		fmt.Sprintf("waiting kind %s", kind),
		func(events []controllerapi.SessionNotification) bool {
			for _, event := range events {
				if event.SessionID != sessionID || event.Notification.Type != sessionevent.NotifyWaiting {
					continue
				}

				for _, item := range event.Notification.Waiting {
					if item.Kind == kind {
						return true
					}
				}
			}

			return false
		},
	)
}

func waitForSubagentSet(
	t *testing.T,
	collector *eventCollector,
	sessionID int64,
	want []int64,
) {
	t.Helper()
	collector.waitFor(
		t,
		fmt.Sprintf("subagent wait set %v", want),
		func(events []controllerapi.SessionNotification) bool {
			for _, event := range events {
				if event.SessionID != sessionID || event.Notification.Type != sessionevent.NotifyWaiting {
					continue
				}

				var got []int64
				for _, item := range event.Notification.Waiting {
					if item.Kind != sessionevent.WaitSubagent {
						return false
					}
					got = append(got, item.ChildID)
				}
				slices.Sort(got)
				if slices.Equal(got, want) {
					return true
				}
			}

			return false
		},
	)
}

func waitForVisibleMessage(
	t *testing.T,
	collector *eventCollector,
	sessionID int64,
	message string,
) {
	t.Helper()
	collector.waitFor(t, message, func(events []controllerapi.SessionNotification) bool {
		return countPublishedMessage(events, sessionID, message) == 1
	})
}

func waitForIdleAfterMessage(
	t *testing.T,
	collector *eventCollector,
	sessionID int64,
	message string,
) {
	t.Helper()
	collector.waitFor(t, "idle after "+message, func(events []controllerapi.SessionNotification) bool {
		messageSeen := false
		for _, event := range events {
			if event.SessionID != sessionID {
				continue
			}

			if event.Notification.Type == sessionevent.NotifyMessage && event.Notification.Message == message {
				messageSeen = true
			}

			if messageSeen && event.Notification.Type == sessionevent.NotifyStateChanged &&
				event.Notification.Status == sessionevent.StateIdle {
				return true
			}
		}

		return false
	})
}
