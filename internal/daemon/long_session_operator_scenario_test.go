package daemon

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/progressruntime"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestHarnessScenario_LongSessionFixturePreservesReportedOrdering(t *testing.T) {
	data, err := os.ReadFile("../testdata/long_session/session_165_sanitized.json")
	require.NoError(t, err)
	var fixture struct {
		Events []struct {
			Kind  string `json:"kind"`
			Count int    `json:"count"`
		} `json:"events"`
	}
	require.NoError(t, json.Unmarshal(data, &fixture))
	require.Len(t, fixture.Events, 7)
	assert.Equal(t, "root_input", fixture.Events[0].Kind)
	assert.Equal(t, "tool_only_responses", fixture.Events[1].Kind)
	assert.Equal(t, 96, fixture.Events[1].Count)
	assert.Equal(t, "model_progress", fixture.Events[2].Kind)
	assert.Equal(t, "compaction", fixture.Events[3].Kind)
	assert.Equal(t, "compaction", fixture.Events[4].Kind)
	assert.Equal(t, "blocking_child_timeout", fixture.Events[5].Kind)
	assert.Equal(t, "terminal_response", fixture.Events[6].Kind)
}

// This synthetic trace preserves only facts documented for production session
// 165. It deliberately does not invent TODO or background-child transitions.
func TestHarnessScenario_LongSessionAcceptsInputWithoutChatReceipt(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	var calls atomic.Int64
	h := newSubagentHarnessWith(t, func(_ string, _ []llmwire.Message) *llmwire.Response {
		if calls.Add(1) == 1 {
			close(entered)
		}

		<-release

		return &llmwire.Response{Text: "first model progress"}
	})
	defer func() {
		close(release)
		h.shutdown()
	}()

	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "sanitized session-165 root input", "fake-model", map[string]any{
		"manager_id": "telegram:main",
	})
	require.NoError(t, err)
	waitForScenarioSignal(t, entered, "model call")
	require.NoError(t, h.mgr.SendToSession(h.ctx, sessionID, "queued follow-up"))

	var inputs, acknowledgements int
	require.NoError(t, h.db.QueryRowContext(h.ctx, `
		SELECT COUNT(*) FROM session_inbox WHERE session_id = ?`,
		sessionID,
	).Scan(&inputs))
	require.NoError(t, h.db.QueryRowContext(h.ctx, `
		SELECT COUNT(*) FROM session_outbox
		WHERE session_id = ? AND type IN ('message_persistent', 'message_replaceable')`,
		sessionID,
	).Scan(&acknowledgements))
	assert.Equal(t, 2, inputs, "initial and queued input must both remain durable")
	assert.Zero(t, acknowledgements, "input acceptance must not become a chat message")
}

func TestHarnessScenario_WorkingMainModelRefreshesProgressEveryThirtySeconds(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	var enteredOnce sync.Once
	h := newSubagentHarnessWith(t, func(_ string, _ []llmwire.Message) *llmwire.Response {
		enteredOnce.Do(func() { close(entered) })
		<-release

		return &llmwire.Response{Text: "late response"}
	})
	collector := collectEvents(h.mgr.PubSub().SubscribeAll())
	defer func() {
		close(release)
		collector.stop()
		h.shutdown()
	}()

	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "sanitized long work", "fake-model", map[string]any{
		"manager_id": "telegram:main",
	})
	require.NoError(t, err)
	waitForScenarioSignal(t, entered, "working main model call")
	progressStore := h.sessStore.(sessionstore.ProgressStore)
	facts, err := progressStore.CaptureProgress(h.ctx, sessionID)
	require.NoError(t, err)
	require.Nil(t, facts.LastSemanticOutputAt)
	require.NotNil(t, facts.EpisodeStartedAt)
	deadline := facts.EpisodeStartedAt.Add(progressruntime.MainModelProgressInterval)

	h.mgr.progress.Reconcile(h.ctx, deadline.Add(-time.Second))
	assert.Equal(t, 0, countSilenceIntents(t, h, sessionID))
	h.mgr.progress.Reconcile(h.ctx, deadline)
	h.mgr.progress.Reconcile(h.ctx, deadline)
	collector.waitFor(t, "working main model progress", func(events []controllerapi.SessionNotification) bool {
		return slices.ContainsFunc(events, func(event controllerapi.SessionNotification) bool {
			return event.SessionID == sessionID && event.Notification.Type == sessionevent.NotifyMessage &&
				strings.Contains(event.Notification.Message, "**🟢 Working**")
		})
	})

	assert.Equal(t, 1, countSilenceIntents(t, h, sessionID),
		"duplicate deadline ticks must reuse one durable progress intent")

	h.mgr.progress.Reconcile(h.ctx, deadline.Add(progressruntime.MainModelProgressInterval))
	assert.Equal(t, 2, countSilenceIntents(t, h, sessionID),
		"an active main model must refresh the card again after another interval")
}

func TestHarnessScenario_ReactivatedEpisodeGetsFullMainModelInterval(t *testing.T) {
	release := make(chan struct{})
	enteredSecond := make(chan struct{})
	var calls atomic.Int64
	h := newSubagentHarnessWith(t, func(_ string, _ []llmwire.Message) *llmwire.Response {
		if calls.Add(1) == 1 {
			return &llmwire.Response{Text: "old final"}
		}

		close(enteredSecond)
		<-release

		return &llmwire.Response{Text: "new final"}
	})
	collector := collectEvents(h.mgr.PubSub().SubscribeAll())
	defer func() {
		close(release)
		collector.stop()
		h.shutdown()
	}()

	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "first episode", "fake-model", map[string]any{
		"manager_id": "telegram:main",
	})
	require.NoError(t, err)
	waitForVisibleMessage(t, collector, sessionID, "old final")
	h.mgr.waitIdle(sessionID)

	old := time.Now().UTC().Add(-time.Hour)
	_, err = h.db.ExecContext(h.ctx, `UPDATE session_outbox SET created_at = ?
		WHERE session_id = ? AND type IN ('message_persistent', 'message_replaceable')`, old, sessionID)
	require.NoError(t, err)
	require.NoError(t, h.mgr.SendToSession(h.ctx, sessionID, "second episode"))
	waitForScenarioSignal(t, enteredSecond, "reactivated model call")

	progressStore := h.sessStore.(sessionstore.ProgressStore)
	facts, err := progressStore.CaptureProgress(h.ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, facts.EpisodeStartedAt)
	require.NotNil(t, facts.LastSemanticOutputAt)
	require.True(t, facts.EpisodeStartedAt.After(*facts.LastSemanticOutputAt))

	h.mgr.progress.Reconcile(
		h.ctx,
		facts.EpisodeStartedAt.Add(progressruntime.MainModelProgressInterval-time.Second),
	)
	assert.Equal(t, 0, countSilenceIntents(t, h, sessionID))

	h.mgr.progress.Reconcile(h.ctx, facts.EpisodeStartedAt.Add(progressruntime.MainModelProgressInterval))
	assert.Equal(t, 1, countSilenceIntents(t, h, sessionID))
}

func TestHarnessScenario_EmptyRootStartsEpisodeWithFirstInput(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	h := newSubagentHarnessWith(t, func(_ string, _ []llmwire.Message) *llmwire.Response {
		close(entered)
		<-release

		return &llmwire.Response{Text: "done"}
	})
	defer func() {
		close(release)
		h.shutdown()
	}()

	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "", "fake-model", map[string]any{
		"manager_id": "telegram:main",
	})
	require.NoError(t, err)

	progressStore := h.sessStore.(sessionstore.ProgressStore)
	roots, err := progressStore.ListAutonomousProgressRoots(h.ctx)
	require.NoError(t, err)
	assert.NotContains(t, roots, sessionID)
	current, err := h.mgr.CurrentProgress(h.ctx, sessionID)
	require.NoError(t, err)
	assert.Contains(t, current.Rendered, "Wall time: unavailable")

	require.NoError(t, h.mgr.SendToSession(h.ctx, sessionID, "first task"))
	waitForScenarioSignal(t, entered, "first model call")

	var episodeStartedAt time.Time
	require.NoError(t, h.db.QueryRowContext(h.ctx,
		`SELECT episode_started_at FROM sessions WHERE id = ?`, sessionID).Scan(&episodeStartedAt))
	assert.False(t, episodeStartedAt.IsZero())

	roots, err = progressStore.ListAutonomousProgressRoots(h.ctx)
	require.NoError(t, err)
	assert.Contains(t, roots, sessionID)
}

func countSilenceIntents(t *testing.T, h *subagentHarness, sessionID int64) int {
	t.Helper()

	var count int
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM session_outbox
		WHERE session_id = ? AND source_key LIKE 'progress:silence:%'`, sessionID).Scan(&count))

	return count
}

func waitForScenarioSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()

	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal(description + " did not start")
	}
}
