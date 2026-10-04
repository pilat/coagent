package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/progressruntime"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/tool"
)

// The first no-wake stop stays hidden; confirmation publishes its candidate text and discards the nudge
// acknowledgement.
func TestHarnessScenario_CompletionCheckConfirmsBeforePublishing(t *testing.T) {
	var calls int
	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		calls++
		if calls == 1 {
			return textReply("premature candidate answer")
		}
		return textReply("why I am stopping")
	}
	h := newHarness(t, harnessOptions{respond: respond})
	collector := collectEvents(t, h.mgr.bus.SubscribeAll())
	defer collector.stop()
	h.startInboxWake()
	root, err := h.mgr.Send(h.ctx, h.projectID, "do the work", "fake-model", managerAttrs(scenarioManagerID))
	require.NoError(t, err)
	collector.waitMessage(root, "premature candidate answer")
	drainScenarioClaims(t, "completion_check_confirmed_final.json", newChainController(t, h))
	collector.waitIdleAfter(root, "premature candidate answer")
	assert.Equal(t, 2, calls, "the no-wake stop costs exactly one confirmation call")
	var ackLeaks, persistent, releases int
	for _, row := range h.outbox(root) {
		content := strings.ToLower(row.Content)
		if strings.Contains(content, "why i am stopping") {
			ackLeaks++
		}
		if row.Type == "message_persistent" {
			if strings.HasPrefix(content, "premature candidate answer") {
				persistent++
			}
			if row.ReleasesInput {
				releases++
			}
		}
	}
	assert.Zero(t, ackLeaks, "the discarded nudge ack must never reach any outbox row")
	assert.Equal(t, 1, persistent, "exactly one persistent manager answer commits: the candidate text")
	assert.Equal(t, 1, releases, "the confirmed output releases the manager input")
	assertHarnessTrace(t, "completion_check_confirmed_final.json", collector.snapshot(), root)
}

// A live background process owns the wake, so a non-empty stop publishes once without a completion nudge.
func TestHarnessScenario_CompletionCheckBackgroundProcessYieldPublishesOnce(t *testing.T) {
	var calls atomic.Int64
	processRunning := make(chan struct{})
	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		n := calls.Add(1)
		if n == 1 {
			// The first stop is decided only after the test has advertised a
			// running process, so the wake projection sees the ledger row.
			<-processRunning
			return textReply("yielding to the running process")
		}
		if hasUserContaining(messages, "<process_completion>") {
			return textReply("resumed after process completion")
		}
		return textReply("follow-up answer")
	}
	h := newHarness(t, harnessOptions{respond: respond})
	collector := collectEvents(t, h.mgr.bus.SubscribeAll())
	defer collector.stop()
	service := installScenarioProcessService(t, h)
	h.startInboxWake()
	root, err := h.mgr.Send(h.ctx, h.projectID, "start a process", "fake-model", managerAttrs(scenarioManagerID))
	require.NoError(t, err)

	// The process holds until the test releases it, so the completion wake lands after the yield has settled.
	release := filepath.Join(t.TempDir(), "release")
	process := startScenarioProcess(t, service, root, root,
		fmt.Sprintf("while [ ! -f %s ]; do sleep 0.05; done; printf 'done\\n'", release))
	func() backgroundprocess.Process {
		var record backgroundprocess.Process
		h.waitUntil("process state", func() bool {
			var err error
			record, err = h.mgr.processStore.GetProcess(context.Background(), process.ID)
			return err == nil && record.State == backgroundprocess.StateRunning
		})
		return record
	}()
	close(processRunning)
	collector.waitMessage(root, "yielding to the running process")
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(root) })
	assert.Equal(t, int64(1), calls.Load(), "a wake yield trusts the stop without a confirmation call")
	var yieldOutputs int
	for _, row := range h.outbox(root) {
		content := strings.ToLower(row.Content)
		if row.Type == "message_persistent" && strings.HasPrefix(content, "🟣 background") &&
			strings.Contains(strings.TrimPrefix(content, "🟣 background"), "yielding to the running process") {
			yieldOutputs++
		}
	}
	assert.Equal(t, 1, yieldOutputs, "the wake yield publishes ordinary output once, opening with the background badge")
	for _, message := range h.messages(root) {
		if message.Role == llmwire.RoleUser {
			assert.NotContains(t, message.Content, "returned a final answer",
				"no completion nudge accompanies a wake yield")
		}
	}
	require.NoError(t, os.WriteFile(release, []byte("go"), 0o644))
	collector.waitMessage(root, "resumed after process completion")
	assert.Equal(t, int64(3), calls.Load(), "the completion wake resumes through the ordinary two-phase check")
}

// An empty stop with the same durable wake source yields immediately too: no
// candidate, no nudge, no empty-streak movement, and one model call only.
func TestHarnessScenario_CompletionCheckEmptyBackgroundYieldYieldsSilently(t *testing.T) {
	var calls atomic.Int64
	processRunning := make(chan struct{})
	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		n := calls.Add(1)
		if n == 1 {
			<-processRunning
			return &llmwire.Response{}
		}
		return textReply("resumed after silent yield")
	}
	h := newHarness(t, harnessOptions{respond: respond})
	collector := collectEvents(t, h.mgr.bus.SubscribeAll())
	defer collector.stop()
	service := installScenarioProcessService(t, h)
	h.startInboxWake()
	root, err := h.mgr.Send(
		h.ctx, h.projectID, "start a process and wait", "fake-model", managerAttrs(scenarioManagerID),
	)
	require.NoError(t, err)
	release := filepath.Join(t.TempDir(), "release")
	process := startScenarioProcess(t, service, root, root,
		fmt.Sprintf("while [ ! -f %s ]; do sleep 0.05; done; printf 'done\\n'", release))
	func() backgroundprocess.Process {
		var record backgroundprocess.Process
		h.waitUntil("process state", func() bool {
			var err error
			record, err = h.mgr.processStore.GetProcess(context.Background(), process.ID)
			return err == nil && record.State == backgroundprocess.StateRunning
		})
		return record
	}()
	close(processRunning)
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(root) })
	assert.Equal(t, int64(1), calls.Load(), "an empty wake yield ends the activation immediately")
	var nudges int
	for _, message := range h.messages(root) {
		if message.Role == llmwire.RoleUser && strings.Contains(message.Content, "empty response") {
			nudges++
		}
	}
	assert.Zero(t, nudges, "an empty wake yield appends no empty-stop nudge")
	require.NoError(t, os.WriteFile(release, []byte("go"), 0o644))
	collector.waitMessage(root, "resumed after silent yield")
}

// A terminal undelivered child promises no wake; only a confirmed second stop may expose the hidden first answer.
func TestHarnessScenario_CompletionCheckStoppedLinkIsNotAWakeSource(t *testing.T) {
	for _, state := range []string{"stopped", "killed"} {
		t.Run(state, func(t *testing.T) {
			var calls atomic.Int64
			linkSeeded := make(chan struct{})
			respond := func(string, []llmwire.Message) *llmwire.Response {
				n := calls.Add(1)
				if n == 1 {
					// The dead link must exist before the first stop decision.
					<-linkSeeded
					return textReply("premature answer over dead child")
				}
				return textReply("confirmed answer over dead child")
			}
			h := newHarness(t, harnessOptions{respond: respond})
			collector := collectEvents(t, h.mgr.bus.SubscribeAll())
			defer collector.stop()
			h.startInboxWake()
			root, err := h.mgr.Send(
				h.ctx, h.projectID, "work while child is dead", "fake-model", managerAttrs(scenarioManagerID),
			)
			require.NoError(t, err)
			h.createChild(root, subagent.Link{TaskCallID: "task-dead", State: subagent.State(state)})
			close(linkSeeded)
			collector.waitMessage(root, "premature answer over dead child")
			assert.Equal(t, int64(2), calls.Load(), "a %s link promises no wake: the two-phase check runs", state)
		})
	}
}

func TestControllerManagerSubscriptionIsExactAcrossRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "routing.db")
	first := newHarness(t, harnessOptions{dbPath: dbPath, respond: trivialRespond})
	firstDB, firstSessions := first.db, first.store
	projectID := testProject(t, first.store, "/tmp/controller-manager-restart")
	record, err := firstSessions.CreateSession(ctx, projectID, "model", "", managerAttrs("manager-7"))
	require.NoError(t, err)
	require.NoError(t, firstDB.Close())
	second := newHarness(t, harnessOptions{dbPath: dbPath, respond: trivialRespond})
	mgr := second.mgr
	controllers := newTestController(mgr, &config.Config{}, nil, nil)
	subscriptions := make(map[string]<-chan controllerapi.SessionNotification, 10)
	for i := range 10 {
		managerID := fmt.Sprintf("manager-%d", i)
		subscriptions[managerID] = controllers.ForManager(managerID).Subscribe()
	}
	mgr.NotifySession(record.ID, sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "after restart"})
	for managerID, subscription := range subscriptions {
		if managerID == "manager-7" {
			notification := requireManagerNotification(t, subscription)
			assert.Equal(t, "after restart", notification.Notification.Message)
			continue
		}
		requireNoManagerNotification(t, subscription)
	}
}

func TestHarnessScenario_SecondInputDoesNotReplayPreviousFinal(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasUserContaining(messages, "second question") {
			return textReply("second answer")
		}
		return textReply("first answer")
	}})
	collector := collectEvents(t, h.mgr.bus.SubscribeAll())
	defer collector.stop()
	h.startInboxWake()
	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "first question", "fake-model", nil)
	require.NoError(t, err)
	collector.waitMessage(sessionID, "first answer")
	collector.waitIdleAfter(sessionID, "first answer")
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, sessionID, "second question"))
	collector.waitMessage(sessionID, "second answer")
	collector.waitIdleAfter(sessionID, "second answer")
	assertHarnessTrace(t, "second_input_no_replay.json", collector.snapshot(), sessionID)
}

func TestHarnessScenario_CLIConversationIsManagerOwned(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: func(string, []llmwire.Message) *llmwire.Response {
		return textReply("configuration answer")
	}})
	collector := collectEvents(t, h.mgr.bus.SubscribeAll())
	defer collector.stop()
	h.startInboxWake()
	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "configure coagent", "fake-model", map[string]any{
		controllerapi.SessionAttributeManagerID: "cli",
		"channel":                               "cli",
	})
	require.NoError(t, err)
	collector.waitMessage(sessionID, "configuration answer")
	assertHarnessTrace(t, "cli_conversation_manager_owned.json", collector.snapshot(), sessionID)
}

func TestHarnessScenario_BackgroundChildCheckpointUpdatesRootCard(t *testing.T) {
	childSecondEntered := make(chan struct{})
	childSecondRelease := make(chan struct{})
	var childSecondOnce sync.Once
	released := false
	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasUserContaining(messages, "CHILD_PROGRESS") {
			if hasToolResultFor(messages, "ls") {
				childSecondOnce.Do(func() { close(childSecondEntered) })
				<-childSecondRelease
				return textReply("background child answer")
			}
			return callReply("child-progress", "ls", `{"path":"."}`)
		}
		if hasUserContaining(messages, "<subagent_completion>") {
			return textReply("background completion delivered")
		}
		if hasToolResultFor(messages, tool.IDSleep) {
			return textReply("background launched; yielded without sleep")
		}
		if hasToolResultFor(messages, tool.IDTask) {
			return callReply(
				"sleep-after-task-result", tool.IDSleep, `{"duration":"1h","reason":"wait for background child"}`,
			)
		}
		return callReply(
			taskCallID, tool.IDTask,
			`{"prompt":"CHILD_PROGRESS","description":"scenario","subagent_type":"general","background":true}`,
		)
	}
	h := newHarness(t, harnessOptions{respond: respond})
	collector := collectEvents(t, h.mgr.bus.SubscribeAll())
	defer func() {
		if !released {
			close(childSecondRelease)
		}
		collector.stop()
		h.shutdown()
	}()
	h.startInboxWake()
	parentID, err := h.mgr.Send(
		h.ctx, h.projectID, "start background child", "fake-model", managerAttrs(scenarioManagerID),
	)
	require.NoError(t, err)
	collector.waitMessage(parentID, "background launched; yielded without sleep")
	link := h.linkByCall(parentID, taskCallID)
	require.NotNil(t, link)
	require.False(t, link.Blocking)
	waitForScenarioSignal(t, childSecondEntered, "child second model call")
	var checkpointCard string
	var checkpointOwner int64
	checkpointSource := fmt.Sprintf("progress:change:subagent:%d:%d:checkpoint:1:g%%", link.ChildID, link.ActivationSeq)
	h.waitUntil("child checkpoint progress card", func() bool {
		var found bool
		for _, row := range h.outbox(parentID) {
			if strings.HasPrefix(row.SourceKey, strings.TrimSuffix(checkpointSource, "%")) {
				checkpointOwner, checkpointCard = row.SessionID, row.Content
				found = true
			}
		}
		return found
	})
	assert.Contains(t, checkpointCard, "iteration ")
	assert.NotContains(t, checkpointCard, "root iteration")
	assert.NotContains(t, checkpointCard, "child iterations")
	assert.NotContains(t, checkpointCard, "tree iterations")
	assert.Equal(t, parentID, checkpointOwner, "checkpoint output must remain root-owned")
	close(childSecondRelease)
	released = true
	collector.waitMessage(parentID, "background completion delivered")
}

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
	h := newHarness(t, harnessOptions{respond: func(_ string, _ []llmwire.Message) *llmwire.Response {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return textReply("first model progress")
	}})
	defer func() {
		close(release)
		h.shutdown()
	}()
	h.startInboxWake()
	sessionID, err := h.mgr.Send(
		h.ctx, h.projectID, "sanitized session-165 root input", "fake-model", managerAttrs("telegram:main"),
	)
	require.NoError(t, err)
	waitForScenarioSignal(t, entered, "model call")
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, sessionID, "queued follow-up"))
	var inputs, acknowledgements int
	require.NoError(t, h.db.QueryRowContext(h.ctx, `
		SELECT COUNT(*) FROM session_inbox WHERE session_id = ?`,
		sessionID,
	).Scan(&inputs))
	for _, row := range h.outbox(sessionID) {
		if row.Type == "message_persistent" || row.Type == "message_replaceable" {
			acknowledgements++
		}
	}
	assert.Equal(t, 2, inputs, "initial and queued input must both remain durable")
	assert.Zero(t, acknowledgements, "input acceptance must not become a chat message")
}

func TestHarnessScenario_WorkingMainModelRefreshesProgressEveryThirtySeconds(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	var enteredOnce sync.Once
	h := newHarness(t, harnessOptions{respond: func(_ string, _ []llmwire.Message) *llmwire.Response {
		enteredOnce.Do(func() { close(entered) })
		<-release
		return textReply("late response")
	}})
	collector := collectEvents(t, h.mgr.bus.SubscribeAll())
	defer func() {
		close(release)
		collector.stop()
		h.shutdown()
	}()
	h.startInboxWake()
	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "sanitized long work", "fake-model", managerAttrs("telegram:main"))
	require.NoError(t, err)
	waitForScenarioSignal(t, entered, "working main model call")
	progressStore := h.store
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
	secondOnce := sync.Once{}
	var calls atomic.Int64
	h := newHarness(t, harnessOptions{respond: func(_ string, _ []llmwire.Message) *llmwire.Response {
		// Episodes one and two each spend a candidate call and a confirming
		// call; only episode two's first call arms the progress probe.
		switch calls.Add(1) {
		case 1, 2:
			return textReply("old final")
		}
		secondOnce.Do(func() { close(enteredSecond) })
		<-release
		return textReply("new final")
	}})
	collector := collectEvents(t, h.mgr.bus.SubscribeAll())
	defer func() {
		close(release)
		collector.stop()
		h.shutdown()
	}()
	h.startInboxWake()
	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "first episode", "fake-model", managerAttrs("telegram:main"))
	require.NoError(t, err)
	collector.waitMessage(sessionID, "old final")
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(sessionID) })
	old := time.Now().UTC().Add(-time.Hour)
	_, err = h.db.ExecContext(h.ctx, `UPDATE session_outbox SET created_at = ?
		WHERE session_id = ? AND type IN ('message_persistent', 'message_replaceable')`, old, sessionID)
	require.NoError(t, err)
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, sessionID, "second episode"))
	waitForScenarioSignal(t, enteredSecond, "reactivated model call")
	progressStore := h.store
	facts, err := progressStore.CaptureProgress(h.ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, facts.EpisodeStartedAt)
	require.NotNil(t, facts.LastSemanticOutputAt)
	require.True(t, facts.EpisodeStartedAt.After(*facts.LastSemanticOutputAt))
	h.mgr.progress.Reconcile(h.ctx, facts.EpisodeStartedAt.Add(progressruntime.MainModelProgressInterval-time.Second))
	assert.Equal(t, 0, countSilenceIntents(t, h, sessionID))
	h.mgr.progress.Reconcile(h.ctx, facts.EpisodeStartedAt.Add(progressruntime.MainModelProgressInterval))
	assert.Equal(t, 1, countSilenceIntents(t, h, sessionID))
}

func TestHarnessScenario_EmptyRootStartsEpisodeWithFirstInput(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	enteredOnce := sync.Once{}
	h := newHarness(t, harnessOptions{respond: func(_ string, _ []llmwire.Message) *llmwire.Response {
		enteredOnce.Do(func() { close(entered) })
		<-release
		return textReply("done")
	}})
	defer func() {
		close(release)
		h.shutdown()
	}()
	h.startInboxWake()
	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "", "fake-model", managerAttrs("telegram:main"))
	require.NoError(t, err)
	progressStore := h.store
	roots, err := progressStore.ListAutonomousProgressRoots(h.ctx)
	require.NoError(t, err)
	assert.NotContains(t, roots, sessionID)
	current, err := h.mgr.progress.Current(h.ctx, sessionID)
	require.NoError(t, err)
	assert.Contains(t, current.Rendered, "Wall time: unavailable")
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, sessionID, "first task"))
	waitForScenarioSignal(t, entered, "first model call")
	var episodeStartedAt time.Time
	require.NoError(t, h.db.QueryRowContext(h.ctx,
		`SELECT episode_started_at FROM sessions WHERE id = ?`, sessionID).Scan(&episodeStartedAt))
	assert.False(t, episodeStartedAt.IsZero())
	roots, err = progressStore.ListAutonomousProgressRoots(h.ctx)
	require.NoError(t, err)
	assert.Contains(t, roots, sessionID)
}

// Three managers share one hidden project but own three independent roots, each bound to its own service topic.
func TestManagementRoot_ThreeManagersShareProjectKeepOwnership(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "mgmt.db")
	h := newHarness(t, harnessOptions{dbPath: dbPath, respond: trivialRespond})
	db := h.db
	cfg := &config.Config{UnifiedConfig: &config.UnifiedConfig{ProjectsRoot: filepath.Join(root, "projects")}}
	svc := h.mgr
	factory := newTestController(svc, cfg, nil, nil)
	const topicBase = 7000
	roots := make(map[string]int64)
	for i, managerID := range []string{"tg-one", "tg-two", "tg-three"} {
		id, err := factory.ForManager(managerID).EnsureManagementRoot(ctx, controllerapi.ManagementRootEnsureData{
			TopicID: int64(topicBase + i),
		})
		require.NoError(t, err)
		require.NotZero(t, id)
		roots[managerID] = id
	}
	assert.Len(t, roots, 3, "three managers own three distinct roots")
	projectIDs := map[int64]bool{}
	for managerID, id := range roots {
		record := h.session(id)
		projectIDs[record.ProjectID] = true
		assert.Equal(t, managerID, record.Attributes[controllerapi.SessionAttributeManagerID])
		assert.Contains(t, record.Attributes, controllerapi.SessionAttributeManagementSurface)
	}
	assert.Len(t, projectIDs, 1, "all management roots share one hidden project")
	var hidden bool
	for projectID := range projectIDs {
		require.NoError(t, db.QueryRowContext(ctx, `SELECT hidden FROM projects WHERE id = ?`, projectID).Scan(&hidden))
		assert.True(t, hidden, "the shared management project is hidden")
		var name string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT name FROM projects WHERE id = ?`, projectID).Scan(&name))
		assert.Equal(t, controllerapi.CoagentManagementProjectDir, name)
	}
}

// Claims protect generation-scoped card replacement; the trace pins follow-up promotion, tool settlement and final
// output.
func TestHarnessScenario_OutputChainReportedOrder(t *testing.T) {
	var calls int
	entered := make(chan struct{})
	followUpQueued := make(chan struct{})
	var once, closeOnce sync.Once
	respond := func(_ string, _ []llmwire.Message) *llmwire.Response {
		calls++
		switch calls {
		case 1:
			// Hold the first turn open until the follow-up is durably queued,
			// so promotion can only happen after the tool settlement.
			once.Do(func() { close(entered) })
			<-followUpQueued

			return &llmwire.Response{
				Text:      "Reading the repo",
				ToolCalls: []llmwire.ToolCall{{ID: "chain-ls", Name: "ls", Arguments: []byte(`{"path":"."}`)}},
			}
		case 2:
			return &llmwire.Response{Text: "Stopping the mutation run", ToolCalls: []llmwire.ToolCall{
				{
					ID:   "chain-todo",
					Name: "todowrite",
					Arguments: []byte(
						`{"todos":[{"id":"t1","content":"ship the change","status":"in_progress","priority":"high"}]}`,
					),
				},
			}}
		default:
			return textReply("All done.")
		}
	}
	h := newHarness(t, harnessOptions{respond: respond})
	defer func() {
		closeOnce.Do(func() { close(followUpQueued) })
		h.shutdown()
	}()

	collector := collectEvents(t, h.mgr.bus.SubscribeAll())
	defer collector.stop()
	h.startInboxWake()
	root, err := h.mgr.Send(h.ctx, h.projectID, "do the work", "fake-model", managerAttrs(scenarioManagerID))
	require.NoError(t, err)
	waitForScenarioSignal(t, entered, "first model call")

	// The follow-up is enqueued while the first tool is unresolved, so it may
	// only enter history after settlement — advancing the generation exactly once.
	_, err = h.store.Enqueue(
		h.ctx, sessionstore.Input{
			SessionID: root,
			Source:    sessionstore.InputSourceUser,
			Content:   "follow-up: also check the docs",
		},
	)
	require.NoError(t, err)
	closeOnce.Do(func() { close(followUpQueued) })
	collector.waitMessage(root, "All done.")
	controller := newChainController(t, h)
	drainScenarioClaims(t, "output_chain_reported_order.json", controller)
	collector.waitIdleAfter(root, "All done.")
	assertHarnessTrace(t, "output_chain_reported_order.json", collector.snapshot(), root)
}

// One manager-owned input keeps its causal obligation through every tool
// iteration while direct-reply eligibility is consumed by the first response.
func TestHarnessScenario_OutputChainNarratedToolIterations(t *testing.T) {
	var calls int
	respond := func(_ string, _ []llmwire.Message) *llmwire.Response {
		calls++
		switch calls {
		case 1:
			return &llmwire.Response{
				Text:      "Reading the repo",
				ToolCalls: []llmwire.ToolCall{{ID: "narrated-ls", Name: "ls", Arguments: []byte(`{"path":"."}`)}},
			}
		case 2:
			return &llmwire.Response{
				Text: "Updating the task list",
				ToolCalls: []llmwire.ToolCall{{
					ID:   "narrated-todo",
					Name: "todowrite",
					Arguments: []byte(
						`{"todos":[{"id":"t1","content":"ship the change","status":"in_progress","priority":"high"}]}`,
					),
				}},
			}
		default:
			return textReply("All done.")
		}
	}
	h := newHarness(t, harnessOptions{respond: respond})

	collector := collectEvents(t, h.mgr.bus.SubscribeAll())
	defer collector.stop()
	h.startInboxWake()
	root, err := h.mgr.Send(h.ctx, h.projectID, "do the work", "fake-model", managerAttrs(scenarioManagerID))
	require.NoError(t, err)
	collector.waitMessage(root, "All done.")
	controller := newChainController(t, h)
	drainScenarioClaims(t, "output_chain_narrated_tools.json", controller)
	collector.waitIdleAfter(root, "All done.")
	assertHarnessTrace(t, "output_chain_narrated_tools.json", collector.snapshot(), root)
}

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
			return textReply("busy process completion observed")
		}
		if hasUserContaining(messages, "start busy process scenario") {
			once.Do(func() { close(entered) })
			<-release
			return callReply("busy-read", "ls", `{"path":"."}`)
		}
		return textReply("unexpected activation")
	}
	h := newHarness(t, harnessOptions{respond: respond})
	service := installScenarioProcessService(t, h)
	collector := collectEvents(t, h.mgr.bus.SubscribeAll())
	defer func() {
		closeOnce(release)
		collector.stop()
		h.shutdown()
	}()
	h.startInboxWake()
	rootID, err := h.mgr.Send(h.ctx, h.projectID, "start busy process scenario", "fake-model", nil)
	require.NoError(t, err)
	<-entered
	process := startScenarioProcess(t, service, rootID, rootID, "printf 'finished\\n'")
	func() backgroundprocess.Process {
		var record backgroundprocess.Process
		h.waitUntil("process state", func() bool {
			var err error
			record, err = h.mgr.processStore.GetProcess(context.Background(), process.ID)
			return err == nil && record.State == backgroundprocess.StateCompleted
		})
		return record
	}()
	close(release)
	collector.waitMessage(rootID, "busy process completion observed")
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
	collector.waitIdleAfter(rootID, "busy process completion observed")
	// The completion wake opened the two-phase check; the confirming stop adds one model call.
	assert.Equal(t, int64(3), modelCalls.Load())
	assertHarnessTrace(t, "process_busy_boundary.json", collector.snapshot(), rootID)
}

func TestHarnessScenario_AgentCancelsOwnedBackgroundProcess(t *testing.T) {
	var processID atomic.Value
	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasToolResultFor(messages, tool.IDCancelProcess) {
			return textReply("unused background process cancelled")
		}
		if hasToolResultFor(messages, "bash") {
			const prefix = "Background process ID (not an operating-system PID): "
			content := ""
			for _, v := range slices.Backward(messages) {
				if v.Role == llmwire.RoleTool && v.ToolName == "bash" {
					content = v.Content
					break
				}
			}

			_, after, found := strings.Cut(content, prefix)
			if !found {
				return textReply("background process ID missing")
			}
			id, _, _ := strings.Cut(after, "\n")
			processID.Store(id)
			return callReply("cancel-background", tool.IDCancelProcess, `{"process_id":"`+id+`"}`)
		}
		return callReply("start-background", "bash", `{"command":"sleep 30","background":true}`)
	}
	h := newHarness(t, harnessOptions{respond: respond})
	service := installScenarioProcessService(t, h)
	h.mgr.build.ProcessService = service
	collector := collectEvents(t, h.mgr.bus.SubscribeAll())
	defer collector.stop()
	h.startInboxWake()
	rootID, err := h.mgr.Send(
		h.ctx, h.projectID, "start then cancel background work", "fake-model", managerAttrs(scenarioManagerID),
	)
	require.NoError(t, err)
	collector.waitMessage(rootID, "unused background process cancelled")
	id, ok := processID.Load().(string)
	require.True(t, ok)
	process, err := h.mgr.processStore.GetProcess(h.ctx, id)
	require.NoError(t, err)
	assert.Equal(t, backgroundprocess.StateCancelled, process.State)
	assert.Equal(t, backgroundprocess.IntentAgentCancelled, process.HostIntent)
	var completions int
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM session_inbox
		WHERE source = 'process' AND json_extract(attributes, '$.process_id') = ?`, id).Scan(&completions))
	assert.Zero(t, completions, "explicit cancellation must not schedule a later completion")
}

func TestHarnessScenario_ProcessCompletionAtIdleTransition(t *testing.T) {
	var modelCalls atomic.Int64
	h := newHarness(t, harnessOptions{respond: func(_ string, messages []llmwire.Message) *llmwire.Response {
		modelCalls.Add(1)
		require.True(t, hasUserContaining(messages, "<process_completion>"))
		return textReply("idle-transition completion observed")
	}})
	collector := collectEvents(t, h.mgr.bus.SubscribeAll())
	defer collector.stop()
	root, err := h.store.CreateSession(h.ctx, h.projectID, "fake-model", "", managerAttrs(scenarioManagerID))
	require.NoError(t, err)
	require.NoError(t, h.store.UpdateSessionStatus(h.ctx, root.ID, sessionstore.SessionStatusCompleted))
	_, err = h.mgr.store.Enqueue(
		h.ctx, sessionstore.Input{
			SessionID:  root.ID,
			Source:     sessionstore.InputSourceProcess,
			Content:    "<process_completion>idle transition</process_completion>",
			Attributes: map[string]any{"process_id": "idle-transition"},
		},
	)
	require.NoError(t, err)
	workDir, err := h.mgr.store.GetProjectWorkDir(h.ctx, h.projectID)
	require.NoError(t, err)
	require.True(t, h.mgr.runners.tryAdmit(false, 0))
	ending := newRunner(func() {}, workDir, root, waitingRunner{sessionID: root.ID}, false)
	_, registered := h.mgr.runners.register(ending)
	require.True(t, registered)
	require.NoError(t, h.mgr.inputReady(h.ctx, root.ID))
	h.mgr.finishRunner(h.ctx, ending, runOutcome{publishIdle: true}, nil)
	collector.waitMessage(root.ID, "idle-transition completion observed")
	drainScenarioClaims(t, "process_idle_transition.json", newChainController(t, h))
	collector.waitIdleAfter(root.ID, "idle-transition completion observed")
	// The confirmed stop costs a second model call; the completion wake itself
	// opened the two-phase check with call one.
	assert.Equal(t, int64(2), modelCalls.Load())
	assertHarnessTrace(t, "process_idle_transition.json", collector.snapshot(), root.ID)
}

func TestHarnessScenario_ProcessCompletionRevivesCompletedRoot(t *testing.T) {
	var calls atomic.Int64
	h := newHarness(t, harnessOptions{respond: func(_ string, messages []llmwire.Message) *llmwire.Response {
		calls.Add(1)
		require.True(t, hasUserContaining(messages, "<process_completion>"))
		return textReply("idle process completion observed")
	}})
	service := installScenarioProcessService(t, h)
	collector := collectEvents(t, h.mgr.bus.SubscribeAll())
	defer collector.stop()
	root := h.createRoot(managerAttrs(scenarioManagerID))
	require.NoError(t, h.store.UpdateSessionStatus(h.ctx, root, sessionstore.SessionStatusCompleted))
	process := startScenarioProcess(t, service, root, root, "printf 'finished\\n'")
	collector.waitMessage(root, "idle process completion observed")
	drainScenarioClaims(t, "process_completed_root.json", newChainController(t, h))
	collector.waitIdleAfter(root, "idle process completion observed")

	// A completion wake hides its first candidate, then confirms a replaceable result that releases the accepted input.
	assert.Equal(t, int64(2), calls.Load())
	assert.Equal(t, 1, countUserCompletions(h.messages(root), "<process_completion>"))
	var persistent, replaceable, releasing int
	for _, row := range h.outbox(root) {
		if !strings.HasPrefix(strings.ToLower(row.Content), "idle process completion observed") {
			continue
		}
		if row.Type == "message_persistent" {
			persistent++
		}
		if row.Type == "message_replaceable" {
			replaceable++
		}
		if row.ReleasesInput {
			releasing++
		}
	}
	assert.Zero(t, persistent, "process-only input must not create a manager direct reply")
	assert.Equal(t, 1, replaceable)
	assert.Equal(t, 1, releasing, "terminal progress still releases the accepted asynchronous input")
	var state string
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT state FROM session_inbox
		WHERE source = 'process' AND json_extract(attributes, '$.process_id') = ?`, process.ID).Scan(&state))
	assert.Equal(t, string(sessionstore.InputStateAccepted), state)
	assertHarnessTrace(t, "process_completed_root.json", collector.snapshot(), root)
}

func TestHarnessScenario_ProcessCompletionInterruptsSleep(t *testing.T) {
	var modelCalls atomic.Int64
	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		modelCalls.Add(1)
		if hasUserContaining(messages, "<process_completion>") {
			return textReply("process interrupted sleep")
		}
		return callReply("process-sleep", tool.IDSleep, `{"duration":"1h","reason":"wait for process"}`)
	}
	h := newHarness(t, harnessOptions{respond: respond})
	service := installScenarioProcessService(t, h)
	collector := collectEvents(t, h.mgr.bus.SubscribeAll())
	defer collector.stop()
	h.startInboxWake()
	rootID, err := h.mgr.Send(h.ctx, h.projectID, "start process sleep", "fake-model", managerAttrs(scenarioManagerID))
	require.NoError(t, err)
	collector.waitWait(rootID, sessionevent.WaitSleep)
	process := startScenarioProcess(t, service, rootID, rootID, "printf 'wake\\n'")
	collector.waitMessage(rootID, "process interrupted sleep")
	drainScenarioClaims(t, "process_interrupts_sleep.json", newChainController(t, h))
	collector.waitIdleAfter(rootID, "process interrupted sleep")
	messages := h.messages(rootID)
	// The completion wake's stop is the hidden candidate; the confirmation adds the second stop of the two-phase check.
	assert.Equal(t, int64(3), modelCalls.Load())
	assert.Equal(t, 1, countUserCompletions(messages, "<process_completion>"))
	assert.Equal(t, 1, countToolResultsFor(messages, tool.IDSleep))
	toolOutput := ""
	for _, v := range slices.Backward(messages) {
		if v.Role == llmwire.RoleTool && v.ToolName == tool.IDSleep {
			toolOutput = v.Content
			break
		}
	}
	assert.Contains(t, toolOutput, "Sleep interrupted")
	assert.Equal(t, backgroundprocess.StateCompleted,
		func() backgroundprocess.Process {
			var record backgroundprocess.Process
			h.waitUntil("process state", func() bool {
				var err error
				record, err = h.mgr.processStore.GetProcess(context.Background(), process.ID)
				return err == nil && record.State == backgroundprocess.StateCompleted
			})
			return record
		}().State)
	assertHarnessTrace(t, "process_interrupts_sleep.json", collector.snapshot(), rootID)
}

func TestHarnessScenario_ProcessCompletionWaitsForForegroundChild(t *testing.T) {
	childRelease := make(chan struct{})
	var parentCalls atomic.Int64
	var missingTaskResult atomic.Bool
	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasUserContaining(messages, "CHILD_PROCESS_BLOCK") {
			<-childRelease
			return textReply("foreground child finished")
		}
		parentCalls.Add(1)
		if hasUserContaining(messages, "<process_completion>") {
			if !hasToolResultFor(messages, tool.IDTask) {
				missingTaskResult.Store(true)
			}
			return textReply("foreground result preceded process input")
		}
		return callReply(
			taskCallID, tool.IDTask, `{"prompt":"CHILD_PROCESS_BLOCK","description":"block","subagent_type":"general"}`,
		)
	}
	h := newHarness(t, harnessOptions{respond: respond})
	service := installScenarioProcessService(t, h)
	collector := collectEvents(t, h.mgr.bus.SubscribeAll())
	defer func() {
		closeOnce(childRelease)
		collector.stop()
		h.shutdown()
	}()
	h.startInboxWake()
	rootID, err := h.mgr.Send(h.ctx, h.projectID, "start foreground process ordering", "fake-model", nil)
	require.NoError(t, err)
	collector.waitWait(rootID, sessionevent.WaitSubagent)
	h.waitUntil("foreground parent parked", func() bool { return !h.mgr.HasActiveLoop(rootID) })
	process := startScenarioProcess(t, service, rootID, rootID, "printf 'queued\\n'")
	func() backgroundprocess.Process {
		var record backgroundprocess.Process
		h.waitUntil("process state", func() bool {
			var err error
			record, err = h.mgr.processStore.GetProcess(context.Background(), process.ID)
			return err == nil && record.State == backgroundprocess.StateCompleted
		})
		return record
	}()
	h.waitUntil("process wake blocked", func() bool {
		pending, err := h.store.PeekPending(h.ctx, rootID)
		return err == nil && pending.Source == sessionstore.InputSourceProcess
	})
	h.waitUntil("process wake runner parked", func() bool { return !h.mgr.HasActiveLoop(rootID) })
	pending, err := h.mgr.store.PeekPending(h.ctx, rootID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.InputSourceProcess, pending.Source)
	assert.Equal(t, int64(1), parentCalls.Load(), "process input cannot cross the foreground task call")
	close(childRelease)
	collector.waitMessage(rootID, "foreground result preceded process input")
	collector.waitIdleAfter(rootID, "foreground result preceded process input")
	messages := h.messages(rootID)
	assert.False(t, missingTaskResult.Load())
	// Process completion arriving during child-wake confirmation must be answered by that same confirming stop.
	assert.Equal(t, int64(3), parentCalls.Load())
	assert.Equal(t, 1, countToolResultsFor(messages, tool.IDTask))
	assert.Equal(t, 1, countUserCompletions(messages, "<process_completion>"))
	assertHarnessTrace(t, "process_waits_for_foreground.json", collector.snapshot(), rootID)
}

func TestProcessCompletionRetainsInputWithoutWakingStoppedOrErroredSession(t *testing.T) {
	for _, status := range []sessionstore.SessionStatus{
		sessionstore.SessionStatusStopped,
		sessionstore.SessionStatusError,
	} {
		t.Run(string(status), func(t *testing.T) {
			var calls atomic.Int64
			h := newHarness(t, harnessOptions{respond: func(string, []llmwire.Message) *llmwire.Response {
				calls.Add(1)
				return textReply("must not run")
			}})
			collector := collectEvents(t, h.mgr.bus.SubscribeAll())
			service := backgroundprocess.NewService(h.mgr.processStore, backgroundprocess.Options{
				OutputDir: t.TempDir(),
			})
			h.mgr.processes = service
			defer collector.stop()
			root := h.createRoot(nil)
			require.NoError(t, h.store.UpdateSessionStatus(h.ctx, root, status))
			observer := &wakeObserver{Store: h.mgr.store, sessionID: root, observed: make(chan struct{})}
			h.mgr.store = observer
			h.startInboxWake()
			process := startScenarioProcess(t, service, root, root, "printf 'parked\\n'")
			h.waitUntil("process input retained", func() bool {
				row, err := h.store.PeekPending(h.ctx, root)
				return err == nil && row.Attributes["process_id"] == process.ID
			})
			select {
			case <-observer.observed:
			case <-time.After(5 * time.Second):
				t.Fatal("inbox wake did not inspect the parked session")
			}
			assert.Never(t, func() bool {
				return h.mgr.HasActiveLoop(root) || calls.Load() != 0 || len(collector.snapshot()) != 0
			}, 100*time.Millisecond, 5*time.Millisecond, "a consumed process wake must leave the session parked")
			assert.False(t, h.mgr.HasActiveLoop(root))
			assert.Zero(t, calls.Load())
			pending, err := h.mgr.store.PeekPending(h.ctx, root)
			require.NoError(t, err)
			assert.Equal(t, sessionstore.InputSourceProcess, pending.Source)
			assert.Equal(t, process.ID, pending.Attributes["process_id"])
			if status == sessionstore.SessionStatusStopped {
				h.startInboxWake()
				require.NoError(t, h.mgr.sendToSession(h.ctx, root, "/help"))
				assert.False(t, h.mgr.HasActiveLoop(root))
				head, peekErr := h.mgr.store.PeekPending(h.ctx, root)
				require.NoError(t, peekErr)
				assert.Equal(t, pending.ID, head.ID)
			}
			assert.Empty(t, collector.snapshot(), "retained completion must emit no controller trace")
		})
	}
}

// Durable routing sends root events only to their owner across cache warming, claims and clear/recreation.
func TestPublishRoutingModel_ManagerOwnershipSurvivesTransitions(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: (&mockFactory{}).client})
	mgr, store := h.mgr, h.store
	ctx := context.Background()
	subscribers := map[string]<-chan controllerapi.SessionNotification{
		"alpha": mgr.bus.SubscribeManager("alpha"),
		"beta":  mgr.bus.SubscribeManager("beta"),
	}
	model := make(map[int64]string)
	pid := testProject(t, store, "/tmp/publish-routing-model")
	create := func(owner string) int64 {
		t.Helper()
		attributes := map[string]any(nil)
		if owner != "" {
			attributes = managerAttrs(owner)
		}
		record, err := mgr.store.CreateSession(ctx, pid, "fake-model", "", attributes)
		require.NoError(t, err)
		model[record.ID] = owner
		return record.ID
	}
	publish := func(sessionID int64, message string) {
		t.Helper()
		mgr.NotifySession(sessionID, sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: message})
		assertModelDeliveries(t, subscribers, model[sessionID], sessionID, message)
	}
	alphaID := create("alpha")
	betaID := create("beta")
	claimedID := create("")
	publish(alphaID, "alpha owns this")
	publish(betaID, "beta owns this")
	publish(claimedID, "nobody owns this")
	require.NoError(t, mgr.SetAttributes(ctx, claimedID, managerAttrs("alpha")))
	model[claimedID] = "alpha"
	publish(claimedID, "alpha claimed the warm route")

	// A restarted daemon begins with empty route caches and must recover the
	// exact owner from the durable session record.
	mgr.routes.mu.Lock()
	mgr.routes.child = make(map[int64]bool)
	mgr.routes.owner = make(map[int64]string)
	mgr.routes.mu.Unlock()
	publish(alphaID, "alpha survives a cold route cache")
	newAlphaID, err := mgr.clear(ctx, lifecycleInput(ctx, t, mgr, alphaID, "/clear"))
	require.NoError(t, err)
	model[newAlphaID] = model[alphaID]
	assertModelDeliveries(t, subscribers, "alpha", alphaID, "")
	publish(newAlphaID, "clear preserved alpha")
}

func TestSpawnedChildProducesNoPubSubEvents(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	collector := collectEvents(t, h.mgr.bus.SubscribeAll())
	defer collector.stop()
	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "spawn a child", "fake-model", nil)
	require.NoError(t, err)

	// Wait for the child to reach a terminal state: its loop — and with it
	// announceSession — must actually have run, or the assertion below is vacuous.
	h.waitUntil("child link", func() bool { return h.linkByCall(parentID, taskCallID) != nil })
	link := *h.linkByCall(parentID, taskCallID)
	h.waitUntil(
		"child delivery",
		func() bool { link := h.link(link.ChildID); return link != nil && link.DeliveredAt != 0 },
	)
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(parentID) })
	var parentEvents int
	for _, sn := range collector.snapshot() {
		assert.NotEqual(t, link.ChildID, sn.SessionID, "child session events must never reach subscribers")
		if sn.SessionID == parentID {
			parentEvents++
		}
	}
	assert.Positive(t, parentEvents, "the parent's own events still flow")
}

// Releasing output must not publish idle while queued user input has already reactivated the root's loop.
func TestReadinessSuppressesIdleWhileRootIsActiveLoop(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, harnessOptions{respond: trivialRespond})
	db, sessions := h.db, h.store
	store := h.store
	projectID := testProject(t, store, "/tmp/readiness-fixture")
	record, err := sessions.CreateSession(ctx, projectID, "model", "", managerAttrs("manager-readiness"))
	require.NoError(t, err)
	sessionID := record.ID
	var outputID int64
	require.NoError(t, db.QueryRow(`INSERT INTO session_outbox
		(session_id, type, content, attributes, source_key, fingerprint, created_at, releases_input, state,
		 attempt_seq, last_attempt_at, delivered_at, last_error)
		VALUES (?, 'message_persistent', 'final', '{}', 'test:final', 'fp', datetime('now'), 1, 'delivered',
		 1, datetime('now'), datetime('now'), '')
		RETURNING id`,
		sessionID).Scan(&outputID))
	mgr := h.mgr
	controllers := newTestController(mgr, &config.Config{}, nil, nil)
	notifications := controllers.ForManager("manager-readiness").Subscribe()
	active := newRunner(func() {}, "", record, waitingRunner{sessionID: record.ID}, false)
	_, registered := registerTestRunner(ctx, mgr, active)
	require.True(t, registered)
	require.NoError(t, mgr.progress.ReconcileOutputReadiness(ctx, outputID))
	requireNoManagerNotification(t, notifications)
	mgr.removeRunner(ctx, active)
	require.False(t, mgr.HasActiveLoop(sessionID))
	require.NoError(t, mgr.progress.ReconcileOutputReadiness(ctx, outputID))
	notification := requireManagerNotification(t, notifications)
	assert.Equal(t, controllerapi.StateIdle, notification.Notification.Status)
	require.NoError(t, mgr.progress.ReconcileOutputReadiness(ctx, outputID))
	mgr.progress.ReconcileLatestReadiness(ctx, record.ID)
	requireNoManagerNotification(t, notifications)
}

// The runner-teardown reconcile must consult the latest releasing output and
// publish idle for it once the live loop is gone.
func TestReconcileLatestReadinessPublishesIdleAfterTeardown(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, harnessOptions{respond: trivialRespond})
	db, sessions := h.db, h.store
	store := h.store
	projectID := testProject(t, store, "/tmp/readiness-fixture")
	record, err := sessions.CreateSession(ctx, projectID, "model", "", managerAttrs("manager-readiness"))
	require.NoError(t, err)
	var outputID int64
	require.NoError(t, db.QueryRow(`INSERT INTO session_outbox
		(session_id, type, content, attributes, source_key, fingerprint, created_at, releases_input, state,
		 attempt_seq, last_attempt_at, delivered_at, last_error)
		VALUES (?, 'message_persistent', 'final', '{}', 'test:final', 'fp', datetime('now'), 1, 'delivered',
		 1, datetime('now'), datetime('now'), '')
		RETURNING id`,
		record.ID).Scan(&outputID))
	mgr := h.mgr
	controllers := newTestController(mgr, &config.Config{}, nil, nil)
	notifications := controllers.ForManager("manager-readiness").Subscribe()
	active := newRunner(func() {}, "", record, waitingRunner{sessionID: record.ID}, false)
	_, registered := registerTestRunner(ctx, mgr, active)
	require.True(t, registered)
	mgr.progress.ReconcileLatestReadiness(ctx, record.ID)
	requireNoManagerNotification(t, notifications)
	mgr.removeRunner(ctx, active)
	require.False(t, mgr.HasActiveLoop(record.ID))
	mgr.progress.ReconcileLatestReadiness(ctx, record.ID)
	notification := requireManagerNotification(t, notifications)
	assert.Equal(t, controllerapi.StateIdle, notification.Notification.Status)
}

func TestOwnerlessIdleIsSuppressedByReplacementRunner(t *testing.T) {
	h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: (&mockFactory{}).client})
	mgr, projects := h.mgr, h.store
	defer mgr.Shutdown(time.Second)
	ctx := context.Background()
	projectID := testProject(t, projects, "/tmp/replacement-idle")
	record, err := mgr.store.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)
	notifications := mgr.bus.SubscribeAll()
	replacement := newRunner(func() {}, "", record, waitingRunner{sessionID: record.ID}, false)
	_, registered := registerTestRunner(ctx, mgr, replacement)
	require.True(t, registered)
	mgr.publishOwnerlessIdle(ctx, record.ID)
	requireNoNotification(t, notifications)
	mgr.removeRunner(ctx, replacement)
	replacement.Complete()
}

func TestHarnessScenario_LengthAttemptIsDiscardedBeforeToolExecution(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: lengthRecoveryResponder(t)})
	collector := collectEvents(t, h.mgr.bus.SubscribeAll())
	defer collector.stop()
	h.startInboxWake()
	sessionID, err := h.mgr.Send(
		h.ctx, h.projectID, "exercise response recovery", "fake-model", managerAttrs(scenarioManagerID),
	)
	require.NoError(t, err)
	collector.waitMessage(sessionID, "recovered complete answer")
	drainScenarioClaims(t, "model_response_length_recovery.json", newChainController(t, h))
	collector.waitIdleAfter(sessionID, "recovered complete answer")
	messages := transcriptOf(h, sessionID)
	assert.Zero(t, countToolResultsFor(messages, "todowrite"))
	assert.Zero(t, countToolResultsFor(messages, "bash"))
	assert.NotContains(t, scenarioTranscriptText(messages), "rejected private fragment")
	assert.NotContains(t, strings.Join(visibleEventMessages(collector.snapshot(), sessionID), "\n"),
		sessionstore.OutputLengthRecoveryPrompt)
	var todoItems string
	require.NoError(t, h.db.QueryRowContext(h.ctx,
		`SELECT todo_items FROM sessions WHERE id = ?`, sessionID).Scan(&todoItems))
	assert.JSONEq(t, `[]`, todoItems, "the valid side-effect probe before the truncated call never executes")
	assertHarnessTrace(t, "model_response_length_recovery.json", collector.snapshot(), sessionID)
}

func TestHarnessScenario_RejectedFinishPublishesCanonicalError(t *testing.T) {
	tests := []struct {
		name       string
		response   llmwire.Response
		prompt     string
		error      string
		trace      string
		sourceTest string
	}{
		{
			name:     "repeated length",
			response: llmwire.Response{Text: "discarded", FinishType: llmwire.FinishLength},
			prompt:   "repeat the limit", error: sessionstore.OutputLengthTerminalError,
			trace:      "model_response_length_exhausted.json",
			sourceTest: "TestHarnessScenario_RepeatedLengthPublishesCanonicalError",
		},
		{
			name: "unknown finish",
			response: llmwire.Response{
				Text: "filtered partial", FinishType: llmwire.FinishUnknown, ProviderFinishReason: "content_filter",
			},
			prompt: "unknown finish", error: sessionstore.UnknownFinishTerminalError,
			trace:      "model_response_unknown_finish.json",
			sourceTest: "TestHarnessScenario_UnknownFinishPublishesCanonicalError",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, harnessOptions{respond: func(string, []llmwire.Message) *llmwire.Response {
				return &tt.response
			}})
			collector := collectEvents(t, h.mgr.bus.SubscribeAll())
			defer collector.stop()
			h.startInboxWake()
			sessionID, err := h.mgr.Send(h.ctx, h.projectID, tt.prompt, "fake-model", managerAttrs(scenarioManagerID))
			require.NoError(t, err)
			want := sessionstore.IntegrityErrorNotice(tt.error)
			collector.waitMessage(sessionID, want)
			drainScenarioClaims(t, tt.trace, newChainController(t, h))
			collector.waitIdleAfter(sessionID, want)
			assert.NotContains(
				t, strings.Join(visibleEventMessages(collector.snapshot(), sessionID), "\n"), tt.response.Text,
			)
			assertHarnessTraceForScenario(t, tt.sourceTest, tt.trace, collector.snapshot(), sessionID)
		})
	}
}

func TestScenario_StartFailureParksWithoutConsumingInput(t *testing.T) {
	h := newHarness(
		t,
		harnessOptions{
			configure: withKnownModels([]string{"removed-model", "working-model"}),
			clientFor: knownModelClient(
				[]string{"removed-model", "working-model"},
				func(string, []llmwire.Message) *llmwire.Response {
					return textReply("done")
				},
			),
		},
	)
	h.startInboxWake()
	id, err := h.mgr.Send(h.ctx, h.projectID, "first", "removed-model", nil)
	require.NoError(t, err)
	h.waitUntil("first answer", func() bool {
		replyCount := 0
		for _, m := range h.messages(id) {
			if m.Role == llmwire.RoleAssistant && m.Content != "" {
				replyCount++
			}
		}
		return replyCount == 2
	})
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(id) })
	h.mgr.build.Config.UnifiedConfig.Models = h.mgr.build.Config.UnifiedConfig.Models[1:]
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, id, "keep this input"))
	h.waitUntil("failure observed", func() bool {
		record, err := h.store.GetSession(h.ctx, id)
		return err == nil && record.Status == sessionstore.SessionStatusError
	})
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(id) })
	record := h.session(id)
	assert.Equal(t, sessionstore.SessionStatusError, record.Status)
	pending, err := h.store.PeekPending(h.ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "keep this input", pending.RawContent)
	require.NoError(t, h.mgr.SetModel(h.ctx, id, "working-model", ""))
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, id, "retry now"))
	h.waitUntil("retry consumes preserved work", func() bool {
		_, err := h.store.PeekPending(h.ctx, id)
		return errors.Is(err, sessionstore.ErrNoPendingInput)
	})
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(id) })
	assert.True(t, hasUserContaining(h.messages(id), "keep this input"))
}

func TestScenario_RepeatedStartFailureCreatesOneOutput(t *testing.T) {
	h := newHarness(
		t,
		harnessOptions{
			configure: withKnownModels([]string{"working-model"}),
			clientFor: knownModelClient([]string{"working-model"}, func(string, []llmwire.Message) *llmwire.Response {
				return textReply("done")
			}),
		},
	)
	record, err := h.store.CreateSession(h.ctx, h.projectID, "removed-model", "", managerAttrs("test-manager"))
	require.NoError(t, err)
	_, err = h.store.Enqueue(
		h.ctx, sessionstore.Input{SessionID: record.ID, Source: sessionstore.InputSourceUser, Content: "work"},
	)
	require.NoError(t, err)
	notifications := h.mgr.bus.Subscribe(record.ID)
	defer h.mgr.bus.Unsubscribe(record.ID, notifications)
	for range 100 {
		h.mgr.reportSessionUnstarted(h.ctx, record.ID, errors.New("model removed-model not found in config"))
	}
	status, err := h.store.OutputQueueStatus(h.ctx, "test-manager")
	require.NoError(t, err)
	assert.Equal(t, 1, status.Pending)
	var notices int
	for len(notifications) > 0 {
		if (<-notifications).Type == sessionevent.NotifyMessage {
			notices++
		}
	}
	assert.Equal(t, 1, notices)
}

func TestScenario_StartFailureRestartKeepsOneReceipt(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "restart.db")
	respond := func(string, []llmwire.Message) *llmwire.Response {
		return textReply("done")
	}
	first := newHarness(
		t, harnessOptions{
			dbPath:    dbPath,
			configure: withKnownModels([]string{"working-model"}),
			clientFor: knownModelClient([]string{"working-model"}, respond),
		},
	)
	t.Cleanup(first.shutdown)
	root, err := first.store.CreateSession(
		first.ctx, first.projectID, "removed-model", "", managerAttrs("test-manager"),
	)
	require.NoError(t, err)
	input, err := first.store.Enqueue(
		first.ctx,
		sessionstore.Input{SessionID: root.ID, Source: sessionstore.InputSourceUser, Content: "preserved work"},
	)
	require.NoError(t, err)
	first.shutdown()

	for range 2 {
		h := newHarness(
			t, harnessOptions{
				dbPath:    dbPath,
				configure: withKnownModels([]string{"working-model"}),
				clientFor: knownModelClient([]string{"working-model"}, respond),
			},
		)
		t.Cleanup(h.shutdown)
		h.startInboxWake()
		h.mgr.resumeAfterRestart(h.ctx)
		h.waitUntil("failed recovery parked", func() bool {
			record, err := h.store.GetSession(h.ctx, root.ID)
			return err == nil && record.Status == sessionstore.SessionStatusError && !h.mgr.HasActiveLoop(root.ID)
		})
		h.shutdown()
		status, err := h.store.OutputQueueStatus(h.ctx, "test-manager")
		require.NoError(t, err)
		assert.Equal(t, 1, status.Pending)
		pending, err := h.store.PeekPending(h.ctx, root.ID)
		require.NoError(t, err)
		assert.Equal(t, input.Input.ID, pending.ID)
	}
}

// Status costs no model turn and cannot claim mid-flight activation; settled tool work still owes a final answer.
func TestHarnessScenario_StatusMidActivationDoesNotStrandJustExecutedToolResults(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasToolResultFor(msgs, "ls") {
			return textReply("work done")
		}

		// Hold the first turn open so /status is durable before the loop reaches
		// the boundary that follows the tool result.
		once.Do(func() { close(entered) })
		<-release
		return callReply("ls-call-1", "ls", `{"path":"."}`)
	}
	h := newHarness(t, harnessOptions{respond: respond})
	collector := collectEvents(t, h.mgr.bus.SubscribeAll())
	released := false
	defer func() {
		if !released {
			close(release)
		}
		collector.stop()
		h.shutdown()
	}()
	h.startInboxWake()
	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "list the workdir", "fake-model", nil)
	require.NoError(t, err)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the model was never asked for the first turn")
	}
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, sessionID, "/status"))
	collector.waitFor(
		t,
		"status answers while the model call is still in flight",
		func(e []controllerapi.SessionNotification) bool {
			return len(statusReports(e, sessionID)) == 1
		},
	)
	close(release)
	released = true
	collector.waitFor(t, "the interrupted work is still answered", func(e []controllerapi.SessionNotification) bool {
		return countPublishedMessage(e, sessionID, "work done") == 1
	})
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(sessionID) })
	events := collector.snapshot()
	assert.Len(t, statusReports(events, sessionID), 1, "exactly one status report")
	assert.Equal(t, 1, countPublishedMessage(events, sessionID, "work done"), "the answer reaches the human once")
	msgs := h.messages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Equal(t, 1, countToolResultsFor(msgs, "ls"))
	assert.Equal(t, "work done", lastAssistantTextDTO(msgs))
	assert.False(t, hasUserContaining(msgs, "/status"), "a control command never enters the transcript")
	h.requireInboxDrained(sessionID)
}

// The opposite edge: with nothing in the session yet, /status must end the
// activation itself rather than hand the provider a conversation asking nothing.
func TestHarnessScenario_StatusOnAFreshSessionCostsNoModelTurn(t *testing.T) {
	rec := &skillRecorder{}
	h := newHarness(t, harnessOptions{respond: rec.wrap(plainRespond)})
	collector := collectEvents(t, h.mgr.bus.SubscribeAll())
	defer collector.stop()
	h.startInboxWake()
	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "/status", "fake-model", nil)
	require.NoError(t, err)
	collector.waitFor(t, "status report reaches the controller", func(e []controllerapi.SessionNotification) bool {
		return len(statusReports(e, sessionID)) == 1
	})
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(sessionID) })
	assert.Len(t, statusReports(collector.snapshot(), sessionID), 1, "exactly one status report")
	assert.Empty(t, rec.snapshot(), "a conversation that asks nothing is never sent to the provider")
	assert.Empty(t, h.messages(sessionID), "the status command writes nothing")
	h.requireInboxDrained(sessionID)
}

// Unlike /compact, /status reads state a blocking child cannot invalidate: it is
// answered on the spot and leaves the pending join untouched.
func TestHarnessScenario_StatusIsAnsweredWhileABlockingChildIsOut(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, harnessOptions{respond: blockingCompactRespond(release)})
	collector := collectEvents(t, h.mgr.bus.SubscribeAll())
	released := false
	defer func() {
		if !released {
			close(release)
		}
		collector.stop()
		h.shutdown()
	}()
	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "do work then spawn", "fake-model", nil)
	require.NoError(t, err)
	h.waitUntil("child link", func() bool { return h.linkByCall(parentID, taskCallID) != nil })
	link := *h.linkByCall(parentID, taskCallID)
	require.True(t, link.Blocking)
	h.waitUntil("parent suspended", func() bool { return !h.mgr.HasActiveLoop(parentID) })
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, parentID, "/status"))
	collector.waitFor(t, "status answered while the child is out", func(e []controllerapi.SessionNotification) bool {
		return len(statusReports(e, parentID)) == 1
	})
	h.waitUntil("status wake finished", func() bool { return !h.mgr.HasActiveLoop(parentID) })
	waiting := h.messages(parentID)
	assert.Equal(t, 1, countAssistantToolCallsFor(waiting, "task"))
	assert.Equal(t, 0, countToolResultsFor(waiting, "task"), "the join must still be owed to the child")
	close(release)
	released = true
	h.waitUntil(
		"child delivery",
		func() bool { link := h.link(link.ChildID); return link != nil && link.DeliveredAt != 0 },
	)
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(parentID) })
	collector.waitFor(t, "the parent answers once the child returns", func(e []controllerapi.SessionNotification) bool {
		return countPublishedMessage(e, parentID, "parent got the child result") == 1
	})
	msgs := h.messages(parentID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Equal(t, 1, countToolResultsFor(msgs, "task"))
	assert.Len(t, statusReports(collector.snapshot(), parentID), 1, "still exactly one status report")
}

// Canonical status ordering resolves priority/time ties by ID and preserves legacy timestamps, icons and the final
// legend.
func TestHarnessScenario_StatusFullTodoListOrderingAndIcons(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: trivialRespond})
	collector := collectEvents(t, h.mgr.bus.SubscribeAll())
	defer collector.stop()
	h.startInboxWake()
	root, err := h.mgr.Send(h.ctx, h.projectID, "plan the work", "fake-model", managerAttrs(scenarioManagerID))
	require.NoError(t, err)
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(root) })
	base := time.Now().UTC().Add(-time.Hour)
	stamp := func(d time.Duration) string { return base.Add(d).Format(time.RFC3339Nano) }
	todos := `[` +
		`{"id":"low","content":"low task","status":"pending","priority":"low","created_at":"` + stamp(0) + `"},` +
		`{"id":"high-late","content":"high late","status":"in_progress","priority":"high","created_at":"` + stamp(time.Minute) + `"},` +
		`{"id":"tie-b","content":"tie b","status":"pending","priority":"high","created_at":"` + stamp(0) + `"},` +
		`{"id":"high-early","content":"high early","status":"completed","priority":"high","created_at":"` + stamp(0) + `"},` +
		`{"id":"tie-a","content":"tie a","status":"cancelled","priority":"high","created_at":"` + stamp(0) + `"},` +
		`{"id":"legacy","content":"legacy item","status":"pending","priority":"medium"}` +
		`]`
	raw := json.RawMessage(todos)
	_, err = h.store.Commit(
		h.ctx, sessionstore.Commit{SessionID: root, State: sessionstore.StatePatch{TodoItems: &raw}},
	)
	require.NoError(t, err)
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, root, "/status"))
	collector.waitFor(t, "full /status TODO list", func(e []controllerapi.SessionNotification) bool {
		return len(statusReports(e, root)) > 0
	})
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(root) })
	reports := statusReports(collector.snapshot(), root)
	report := reports[len(reports)-1]
	assert.Contains(t, report, "- TODO: 1 active · 4 remaining · 1 done · 1 cancelled")
	// Canonical order: priority, then created_at, then ID for exact ties.
	assert.Equal(t, []string{
		"  - ✅ high early",
		"  - 🚫 tie a",
		"  - ⏳ tie b",
		"  - 🔄 high late",
		"  - ⏳ legacy item",
		"  - ⏳ low task",
	}, todoRows(report))
	// Priority is ordering-only metadata, never visible text.
	assert.NotContains(t, report, "[high]")
	assert.NotContains(t, report, "[medium]")
	assert.NotContains(t, report, "[low]")
	assert.NotContains(t, report, "[in_progress]")
	// The legend follows the rows after one blank line.
	assert.Contains(t, report,
		"  - ⏳ low task\n\nLegend: ⏳ pending · 🔄 in progress · ✅ completed · 🚫 cancelled")
	h.requireInboxDrained(root)
}

// Read-only status commands answer a stopped root without reactivating it or changing its durable status.
func TestScenario_StoppedRootAnswersStatusWithoutReactivating(t *testing.T) {
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "what happened?") {
			return textReply("fresh response")
		}
		if countAssistantToolCallsFor(msgs, "bash") > 0 {
			return textReply("old work was interrupted")
		}
		return callReply("long-bash", "bash", `{"command":"sleep 30"}`)
	}
	h := newHarness(t, harnessOptions{respond: respond})
	collector := collectEvents(t, h.mgr.bus.SubscribeAll())
	defer collector.stop()
	h.startInboxWake()
	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "watch checks", "fake-model", nil)
	require.NoError(t, err)
	h.waitUntil("ordinary bash started", func() bool {
		return countAssistantToolCallsFor(h.messages(sessionID), "bash") == 1 &&
			h.mgr.HasActiveLoop(sessionID)
	})
	require.NoError(t, h.mgr.sendToSession(h.ctx, sessionID, "/stop"))
	h.waitUntil("stop completed", func() bool {
		rec, getErr := h.store.GetSession(h.ctx, sessionID)
		return getErr == nil && rec.Status == sessionstore.SessionStatusStopped
	})
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, sessionID, "/status"))
	collector.waitFor(t, "status report reaches the controller", func(e []controllerapi.SessionNotification) bool {
		return len(statusReports(e, sessionID)) == 1
	})
	collector.waitFor(t, "status processing becomes idle", func(e []controllerapi.SessionNotification) bool {
		return slices.ContainsFunc(e, func(event controllerapi.SessionNotification) bool {
			return event.SessionID == sessionID &&
				event.Notification.Type == sessionevent.NotifyStateChanged &&
				event.Notification.Status == controllerapi.StateIdle &&
				event.Notification.Reason == ""
		})
	})
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(sessionID) })
	assert.Len(t, statusReports(collector.snapshot(), sessionID), 1, "the stopped root answers /status")
	rec := h.session(sessionID)
	assert.Equal(t, sessionstore.SessionStatusStopped, rec.Status,
		"a read-only command must not reactivate the stopped root")
	idle := lastIdleStatus(collector.snapshot(), sessionID)
	if idle != nil {
		assert.Empty(t, idle.Reason, "read-only processing must not be mistaken for a lifecycle stop")
	}
}

func countSilenceIntents(t *testing.T, h *harness, sessionID int64) int {
	t.Helper()
	var count int
	for _, row := range h.outbox(sessionID) {
		if strings.HasPrefix(row.SourceKey, "progress:silence:") {
			count++
		}
	}
	return count
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

func assertModelDeliveries(
	t *testing.T,
	subscribers map[string]<-chan controllerapi.SessionNotification,
	wantOwner string,
	wantSessionID int64,
	wantMessage string,
) {
	t.Helper()
	var gotOwners []string
	for managerID, channel := range subscribers {
		select {
		case notification := <-channel:
			gotOwners = append(gotOwners, managerID)
			assert.Equal(t, wantSessionID, notification.SessionID)
			if wantMessage != "" {
				assert.Equal(t, wantMessage, notification.Notification.Message)
			}
		default:
		}
	}
	slices.Sort(gotOwners)
	wantOwners := []string(nil)
	if wantOwner != "" {
		wantOwners = []string{wantOwner}
	}
	assert.Equal(t, wantOwners, gotOwners)
}

func visibleEventMessages(events []controllerapi.SessionNotification, sessionID int64) []string {
	var messages []string
	for _, event := range events {
		if event.SessionID == sessionID && event.Notification.Message != "" {
			messages = append(messages, event.Notification.Message)
		}
	}
	return messages
}

func registerTestRunner(ctx context.Context, manager *svc, rs *runner) (*runner, bool) {
	if !manager.runners.tryAdmit(rs.child, rs.parentID) {
		return nil, false
	}
	manager.liveMu.Lock()
	defer manager.liveMu.Unlock()
	manager.progress.SetLive(rs.sessionID, progressruntime.Live{Active: true})
	existing, registered := manager.runners.register(rs)
	if !registered {
		manager.runners.release(rs.child, rs.parentID)
		manager.updateLiveLocked(ctx, rs.sessionID)
	}
	return existing, registered
}

// The status header a controller receives, duplicated from internal/session on
// purpose: it is the contract with the human, not an implementation detail.
const statusReportHeader = "## Session progress"

func statusReports(events []controllerapi.SessionNotification, sessionID int64) []string {
	var out []string
	for _, event := range events {
		if event.SessionID != sessionID || event.Notification.Type != sessionevent.NotifyMessage {
			continue
		}
		if strings.HasPrefix(event.Notification.Message, statusReportHeader) {
			out = append(out, event.Notification.Message)
		}
	}
	return out
}

// todoRows extracts the icon-only TODO rows from a rendered status report.
func todoRows(report string) []string {
	var rows []string
	for line := range strings.SplitSeq(report, "\n") {
		for _, icon := range []string{"⏳", "🔄", "✅", "🚫", "❔"} {
			if strings.HasPrefix(line, "  - "+icon+" ") {
				rows = append(rows, line)
			}
		}
	}
	return rows
}

func lastIdleStatus(events []controllerapi.SessionNotification, sessionID int64) *sessionevent.Notification {
	for _, event := range slices.Backward(events) {
		if event.SessionID == sessionID && event.Notification.Type == sessionevent.NotifyStateChanged &&
			event.Notification.Status == controllerapi.StateIdle {
			n := event.Notification
			return &n
		}
	}
	return nil
}

type wakeObserver struct {
	Store
	sessionID int64
	observed  chan struct{}
	once      sync.Once
}

func (s *wakeObserver) GetSession(ctx context.Context, id int64) (*sessionstore.SessionRecord, error) {
	record, err := s.Store.GetSession(ctx, id)
	if id == s.sessionID {
		s.once.Do(func() { close(s.observed) })
	}
	return record, err
}

func lengthRecoveryResponder(t *testing.T) func(string, []llmwire.Message) *llmwire.Response {
	t.Helper()
	var calls int
	return func(_ string, messages []llmwire.Message) *llmwire.Response {
		calls++
		if calls > 1 {
			visible := scenarioTranscriptText(messages)
			require.NotContains(t, visible, "rejected private fragment")
			require.Contains(t, visible, sessionstore.OutputLengthRecoveryPrompt)
			return &llmwire.Response{Text: "recovered complete answer", FinishType: llmwire.FinishStop}
		}
		return &llmwire.Response{
			Text: "rejected private fragment", FinishType: llmwire.FinishLength,
			ProviderFinishReason: "length", CostUSD: 0.5,
			Usage: &llmwire.MessageUsage{PromptTokens: 100, CompletionTokens: 200},
			ToolCalls: []llmwire.ToolCall{
				{
					ID: "side-effect-probe", Name: "todowrite",
					Arguments: []byte(
						`{"items":[{"id":"must-not-run","content":"must not run","status":"in_progress","priority":"high"}]}`,
					),
				},
				{ID: "truncated-call", Name: "bash", Arguments: []byte(`{"command":"` + strings.Repeat("x", 128*1024))},
			},
		}
	}
}
