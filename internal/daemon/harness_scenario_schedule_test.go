package daemon

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionbus"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/tool"
)

type scheduleRestartHarness struct {
	*subagentHarness
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

func dropTransientEvents(events []controllerapi.SessionNotification) []controllerapi.SessionNotification {
	kept := make([]controllerapi.SessionNotification, 0, len(events))

	for _, event := range events {
		if event.Notification.Type == sessionevent.NotifyStateChanged ||
			event.Notification.Type == sessionevent.NotifySessionCreated {
			continue
		}

		kept = append(kept, event)
	}

	return kept
}

func scheduleRestartResponder(release <-chan struct{}) func(string, []llmwire.Message) *llmwire.Response {
	return func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasToolResultFor(messages, tool.IDSchedule) {
			<-release

			return &llmwire.Response{Text: "scheduled work completed"}
		}

		return &llmwire.Response{Text: "ready for schedule"}
	}
}

func deliverOneShotBeforeRestart(
	t *testing.T,
	h *scheduleRestartHarness,
	release chan<- struct{},
) (int64, *eventCollector) {
	t.Helper()
	events := collectEvents(h.mgr.PubSub().SubscribeManager("telegram-main"))
	t.Cleanup(events.stop)
	parentID := createScheduleSession(t, h, events)
	flaky := addFlakyDueOneShot(t, h, parentID)
	observer := newScheduleRunningObserver(t, h.mgr.PubSub())
	sender := &orderedScheduleSender{SessionSender: h.mgr, running: observer.running, done: observer.done}
	executor := schedule.NewExecutor(flaky, sender)
	executor.Start(h.ctx)
	t.Cleanup(executor.Stop)

	waitForScheduledInput(t, events, parentID)
	requireSignal(t, flaky.attempted)
	close(release)
	waitForVisibleMessage(t, events, parentID, "scheduled work completed")
	executor.Stop()

	requireOneShotRemainsRetryable(t, h, parentID)
	return parentID, events
}

func createScheduleSession(t *testing.T, h *scheduleRestartHarness, events *eventCollector) int64 {
	t.Helper()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "initialize", "fake-model", map[string]any{
		controllerapi.SessionAttributeManagerID: "telegram-main",
	})
	require.NoError(t, err)
	waitForVisibleMessage(t, events, parentID, "ready for schedule")

	return parentID
}

func addFlakyDueOneShot(t *testing.T, h *scheduleRestartHarness, sessionID int64) *failFirstRemoveScheduleStore {
	t.Helper()
	due := time.Now().Add(-time.Minute).UTC()
	_, err := h.schedStore.AddSchedule(h.ctx, sessionID, "", &due, "scheduled once", false)
	require.NoError(t, err)

	return &failFirstRemoveScheduleStore{Store: h.schedStore, attempted: make(chan struct{})}
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
	remaining, err := h.schedStore.ListSchedules(h.ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, remaining, 1, "failed acknowledgement must leave the accepted one-shot retryable")
	assert.Equal(t, 1, countToolResultsFor(h.parentMessages(sessionID), tool.IDSchedule))
}

func retryOneShotAfterRestart(
	t *testing.T,
	h *scheduleRestartHarness,
	sessionID int64,
) *eventCollector {
	t.Helper()
	// Subscribe before Start: recovery announces the resumed runner, and a
	// subscription that loses that race drops the session_created trace event.
	events := collectEvents(h.mgr.PubSub().SubscribeManager("telegram-main"))
	t.Cleanup(events.stop)
	require.NoError(t, h.mgr.Start(h.ctx))
	executor := schedule.NewExecutor(h.schedStore, h.mgr)
	executor.Start(h.ctx)
	t.Cleanup(executor.Stop)

	require.Eventually(t, func() bool {
		schedules, err := h.schedStore.ListSchedules(h.ctx, sessionID)
		return err == nil && len(schedules) == 0 && !h.mgr.HasActiveLoop(sessionID)
	}, 5*time.Second, 10*time.Millisecond, "restart retry must acknowledge the accepted one-shot")
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
	db := openScheduleRestartDB(t, dbPath)
	h := &scheduleRestartHarness{db: db}
	h.subagentHarness = buildScheduleRestartHarness(t, db, workDir, respond)
	t.Cleanup(func() { require.NoError(t, h.close()) })

	return h
}

func openScheduleRestartDB(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	db, err := migrate.OpenDB(context.Background(), dbPath)
	require.NoError(t, err)
	require.NoError(t, migrate.Run(context.Background(), db, dbPath))

	return db
}

func buildScheduleRestartHarness(
	t *testing.T,
	db *sql.DB,
	workDir string,
	respond func(string, []llmwire.Message) *llmwire.Response,
) *subagentHarness {
	t.Helper()
	store := NewStore(db)
	sessionStore := sessionstore.NewStore(db)
	links := subagent.NewStore(db)
	schedules := schedule.NewStore(db)
	factory := scheduleRestartFactory(workDir, sessionStore, respond)
	mgr := newSvc(
		factory, store, sessionStore, sessionStore, links, subagent.NewTransactions(db),
		budget.New(sessionStore), schedule.NewService(schedules), func() string {
			return "fake-model"
		})
	projectID, err := store.GetOrCreateProject(context.Background(), workDir)
	require.NoError(t, err)

	return &subagentHarness{
		t: t, mgr: mgr, sessStore: sessionStore, links: links, schedStore: schedules,
		projectID: projectID, ctx: context.Background(),
	}
}

func scheduleRestartFactory(
	workDir string,
	store sessionstore.Store,
	respond func(string, []llmwire.Message) *llmwire.Response,
) session.Factory {
	cfg := &config.Config{WorkDir: workDir, Model: "fake-model"}
	return session.NewFactoryWithOptions(
		cfg, nil, nil, store, nil, nil, nil, nil, nil,
		session.WithLLMClientFactory(func(*config.Config) (llm.Client, error) {
			return &scriptedLLM{respond: respond}, nil
		}),
	)
}

func (h *scheduleRestartHarness) close() error {
	h.closeOnce.Do(func() {
		h.shutdown()
		h.closeErr = h.db.Close()
	})

	return h.closeErr
}
