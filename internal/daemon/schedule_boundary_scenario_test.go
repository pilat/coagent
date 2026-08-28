package daemon

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

type stoppedRootScheduleCase struct {
	name, prompt, answer, trace string
	fresh                       bool
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

func runStoppedRootScheduleScenario(t *testing.T, tc stoppedRootScheduleCase) {
	t.Helper()
	started := make(chan struct{})
	release := make(chan struct{})
	h, rootID, collector := newStoppedRootScheduleHarness(t, tc, started, release)
	oldEpisode := time.Now().UTC().Add(-time.Hour)
	_, err := h.db.ExecContext(t.Context(),
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

func newStoppedRootScheduleHarness(
	t *testing.T,
	tc stoppedRootScheduleCase,
	started chan<- struct{},
	release <-chan struct{},
) (*subagentHarness, int64, *eventCollector) {
	t.Helper()
	h := newSubagentHarnessWith(t, stoppedRootScheduleResponder(tc, started, release))
	t.Cleanup(h.shutdown)

	rootID, err := h.mgr.Send(t.Context(), h.projectID, "initialize", "fake-model", map[string]any{
		controllerapi.SessionAttributeManagerID: "telegram-main",
	})
	require.NoError(t, err)
	h.mgr.waitIdle(rootID)
	require.NoError(t, h.mgr.Stop(t.Context(), rootID))
	collector := collectEvents(h.mgr.PubSub().SubscribeManager("telegram-main"))
	t.Cleanup(collector.stop)

	return h, rootID, collector
}

func stoppedRootScheduleResponder(
	tc stoppedRootScheduleCase,
	started chan<- struct{},
	release <-chan struct{},
) func(string, []llmwire.Message) *llmwire.Response {
	return func(_ string, messages []llmwire.Message) *llmwire.Response {
		if scheduledTurnRequested(tc, messages) {
			close(started)
			<-release

			return &llmwire.Response{Text: tc.answer}
		}

		return &llmwire.Response{Text: "ready"}
	}
}

func scheduledTurnRequested(tc stoppedRootScheduleCase, messages []llmwire.Message) bool {
	if tc.fresh {
		return hasUserContaining(messages, tc.prompt)
	}

	return hasToolResultFor(messages, tool.IDSchedule)
}

func runDueStoppedRootSchedule(
	t *testing.T,
	h *subagentHarness,
	rootID int64,
	collector *eventCollector,
	tc stoppedRootScheduleCase,
	started <-chan struct{},
	release chan<- struct{},
) string {
	t.Helper()
	due := time.Now().Add(-time.Minute).UTC()
	entry, err := h.schedStore.AddSchedule(t.Context(), rootID, "", &due, tc.prompt, tc.fresh)
	require.NoError(t, err)

	observer := newScheduleRunningObserver(t, h.mgr.PubSub())
	sender := &orderedScheduleSender{SessionSender: h.mgr, running: observer.running, done: observer.done}
	executor := schedule.NewExecutor(h.schedStore, sender)
	executor.Start(t.Context())
	t.Cleanup(executor.Stop)
	requireSignal(t, started)
	assertStoppedRootActive(t, h, rootID)
	close(release)

	require.Eventually(t, func() bool {
		entries, listErr := h.schedStore.ListSchedules(t.Context(), rootID)
		return listErr == nil && len(entries) == 0 && !h.mgr.HasActiveLoop(rootID) &&
			lastAssistantTextDTO(h.parentMessages(rootID)) == tc.answer
	}, 5*time.Second, 10*time.Millisecond)
	executor.Stop()
	waitForVisibleMessage(t, collector, rootID, tc.answer)

	return fmt.Sprintf("schedule:one-shot:%d", entry.ID())
}

func assertStoppedRootActive(t *testing.T, h *subagentHarness, rootID int64) {
	t.Helper()
	rec, err := h.sessStore.GetSession(t.Context(), rootID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.SessionStatusActive, rec.Status)
}

func assertStoppedRootScheduleResult(t *testing.T, h *subagentHarness, rootID int64, tc stoppedRootScheduleCase) {
	t.Helper()
	messages := h.parentMessages(rootID)
	if tc.fresh {
		assert.Equal(t, 1, countMessageContentContaining(messages, tc.prompt))
		assert.Equal(t, 1, countMessageContentContaining(messages, tc.answer))

		return
	}

	assert.Equal(t, 1, countToolResultsFor(messages, tool.IDSchedule))
}

func assertStoppedRootScheduleDuplicate(
	t *testing.T,
	h *subagentHarness,
	rootID int64,
	deliveryID string,
	tc stoppedRootScheduleCase,
	episodeStartedAt time.Time,
) {
	t.Helper()
	require.NoError(t, h.mgr.Stop(t.Context(), rootID))
	applied, err := deliverStoppedRootSchedule(t, h.mgr, rootID, deliveryID, tc)
	require.NoError(t, err)
	assert.False(t, applied, "an acknowledged retry must not create another turn")
	require.Eventually(t, func() bool { return !h.mgr.HasActiveLoop(rootID) }, time.Second, 10*time.Millisecond)

	rec, err := h.sessStore.GetSession(t.Context(), rootID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.SessionStatusStopped, rec.Status)
	assert.Equal(t, episodeStartedAt, sessionEpisodeStart(t, h, rootID))
	assertStoppedRootScheduleResult(t, h, rootID, tc)
	_, err = h.mgr.DeliverPendingCallResult(t.Context(), rootID, "missing-call", tool.IDSleep, "must stay stopped")
	require.ErrorContains(t, err, "stopped")
}

func sessionEpisodeStart(t *testing.T, h *subagentHarness, rootID int64) time.Time {
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
		return mgr.DeliverFreshSchedule(t.Context(), rootID, deliveryID, tc.prompt)
	}

	return mgr.DeliverScheduleTick(t.Context(), rootID, deliveryID, tc.prompt)
}
