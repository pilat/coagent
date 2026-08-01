package schedule_test

import (
	"context"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/daemon"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

type mockSender struct {
	mu            sync.Mutex
	calls         []sendCall
	notifications []sessionevent.Notification
	sendErr       error
}

type sendCall struct {
	sessionID  int64
	kind       string
	deliveryID string
	callID     string
	toolName   string
	input      string
}

func (m *mockSender) DeliverPendingCallResult(
	_ context.Context,
	sessionID int64,
	callID, toolName, content string,
) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, sendCall{
		sessionID: sessionID,
		kind:      "pending_result",
		callID:    callID,
		toolName:  toolName,
		input:     content,
	})
	return m.sendErr == nil, m.sendErr
}

func (m *mockSender) DeliverScheduleTick(
	_ context.Context, sessionID int64, deliveryID, content string,
) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, sendCall{
		sessionID: sessionID, kind: "tick", deliveryID: deliveryID, input: content,
	})
	return m.sendErr == nil, m.sendErr
}

func (m *mockSender) DeliverFreshSchedule(
	_ context.Context, sessionID int64, deliveryID, content string,
) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, sendCall{
		sessionID: sessionID, kind: "fresh", deliveryID: deliveryID, input: content,
	})
	return m.sendErr == nil, m.sendErr
}

func (m *mockSender) NotifySession(_ int64, notification sessionevent.Notification) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notifications = append(m.notifications, notification)
}

func (m *mockSender) getCalls() []sendCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]sendCall{}, m.calls...)
}

func (m *mockSender) getNotifications() []sessionevent.Notification {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]sessionevent.Notification{}, m.notifications...)
}

func newTestDB(t *testing.T) (schedule.Store, daemon.Store, sessionstore.Store) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := migrate.OpenDB(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	require.NoError(t, migrate.Run(context.Background(), db, dbPath))
	return schedule.NewStore(db), daemon.NewStore(db), sessionstore.NewStore(db)
}

func TestExecutor_OneShotFires(t *testing.T) {
	schedStore, daemonStore, sessStore := newTestDB(t)
	sender := &mockSender{}

	pid, err := daemonStore.GetOrCreateProject(context.Background(), "/tmp/test")
	require.NoError(t, err)
	rec, err := sessStore.CreateSession(context.Background(), pid, "", "", nil)
	require.NoError(t, err)

	past := time.Now().Add(-time.Minute).UTC()
	_, err = schedStore.AddScheduleWithMeta(
		context.Background(),
		rec.ID,
		"",
		&past,
		"wake up and check",
		false,
		schedule.ScheduleMetadata{ToolCallID: "sleep-call-1"},
	)
	require.NoError(t, err)

	executor := schedule.NewExecutor(schedStore, sender)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	executor.Start(ctx)

	time.Sleep(500 * time.Millisecond)
	executor.Stop()

	calls := sender.getCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, rec.ID, calls[0].sessionID)
	assert.Equal(t, "pending_result", calls[0].kind)
	assert.Equal(t, "sleep-call-1", calls[0].callID)
	assert.Equal(t, tool.IDSleep, calls[0].toolName)
	assert.Equal(t, "wake up and check", calls[0].input)

	schedules, err := schedStore.ListSchedules(context.Background(), rec.ID)
	require.NoError(t, err)
	assert.Empty(t, schedules)
}

func TestExecutor_StandaloneOneShotDeliversFutureInput(t *testing.T) {
	schedStore, daemonStore, sessStore := newTestDB(t)
	sender := &mockSender{}

	pid, err := daemonStore.GetOrCreateProject(context.Background(), "/tmp/test")
	require.NoError(t, err)
	rec, err := sessStore.CreateSession(context.Background(), pid, "", "", nil)
	require.NoError(t, err)

	past := time.Now().Add(-time.Minute).UTC()
	standalone, err := schedStore.AddSchedule(
		context.Background(), rec.ID, "", &past, "one-time check", false,
	)
	require.NoError(t, err)

	executor := schedule.NewExecutor(schedStore, sender)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	executor.Start(ctx)
	time.Sleep(500 * time.Millisecond)
	executor.Stop()

	calls := sender.getCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, "tick", calls[0].kind)
	assert.Equal(t, "schedule:one-shot:"+strconv.FormatInt(standalone.ID(), 10), calls[0].deliveryID)
	assert.Equal(t, "one-time check", calls[0].input)

	notifications := sender.getNotifications()
	require.Len(t, notifications, 1)
	assert.Equal(t, sessionevent.NotifyInputReceived, notifications[0].Type)
	assert.Equal(t, "scheduler", notifications[0].Source)
	assert.Equal(t, "one-time check", notifications[0].Message)

	schedules, err := schedStore.ListSchedules(context.Background(), rec.ID)
	require.NoError(t, err)
	assert.Empty(t, schedules, "accepted standalone one-shot is acknowledged by deletion")
}

func TestExecutor_FreshStandaloneOneShotUsesFreshDelivery(t *testing.T) {
	schedStore, daemonStore, sessStore := newTestDB(t)
	sender := &mockSender{}

	pid, err := daemonStore.GetOrCreateProject(context.Background(), "/tmp/test")
	require.NoError(t, err)
	rec, err := sessStore.CreateSession(context.Background(), pid, "", "", nil)
	require.NoError(t, err)

	past := time.Now().Add(-time.Minute).UTC()
	standalone, err := schedStore.AddSchedule(
		context.Background(), rec.ID, "", &past, "fresh one-time work", true,
	)
	require.NoError(t, err)

	executor := schedule.NewExecutor(schedStore, sender)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	executor.Start(ctx)
	t.Cleanup(executor.Stop)

	require.Eventually(t, func() bool { return len(sender.getCalls()) == 1 }, time.Second, 10*time.Millisecond)
	calls := sender.getCalls()
	assert.Equal(t, "fresh", calls[0].kind)
	assert.Equal(t, "schedule:one-shot:"+strconv.FormatInt(standalone.ID(), 10), calls[0].deliveryID)
	assert.Equal(t, "fresh one-time work", calls[0].input)
}

func TestExecutor_CronFires(t *testing.T) {
	schedStore, daemonStore, sessStore := newTestDB(t)
	sender := &mockSender{}

	pid, err := daemonStore.GetOrCreateProject(context.Background(), "/tmp/test")
	require.NoError(t, err)
	rec, err := sessStore.CreateSession(context.Background(), pid, "", "", nil)
	require.NoError(t, err)

	_, err = schedStore.AddSchedule(context.Background(), rec.ID, "* * * * *", nil, "periodic check", false)
	require.NoError(t, err)

	executor := schedule.NewExecutor(schedStore, sender)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	executor.Start(ctx)

	time.Sleep(500 * time.Millisecond)
	executor.Stop()

	calls := sender.getCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, "tick", calls[0].kind)
	assert.Contains(t, calls[0].input, "Schedule tick #1")
	assert.Contains(t, calls[0].input, "periodic check")

	schedules, err := schedStore.ListSchedules(context.Background(), rec.ID)
	require.NoError(t, err)
	require.Len(t, schedules, 1)
	assert.NotNil(t, schedules[0].LastFiredAt(), "a delivered tick is durably acknowledged")
}

func TestExecutor_CronDeliveryFailureIsNotAcknowledged(t *testing.T) {
	schedStore, daemonStore, sessStore := newTestDB(t)
	sender := &mockSender{sendErr: assert.AnError}

	pid, err := daemonStore.GetOrCreateProject(context.Background(), "/tmp/test")
	require.NoError(t, err)
	rec, err := sessStore.CreateSession(context.Background(), pid, "", "", nil)
	require.NoError(t, err)

	_, err = schedStore.AddSchedule(context.Background(), rec.ID, "* * * * *", nil, "retry me", false)
	require.NoError(t, err)

	executor := schedule.NewExecutor(schedStore, sender)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	executor.Start(ctx)
	time.Sleep(500 * time.Millisecond)
	executor.Stop()

	require.Len(t, sender.getCalls(), 1)
	schedules, err := schedStore.ListSchedules(context.Background(), rec.ID)
	require.NoError(t, err)
	require.Len(t, schedules, 1)
	assert.Nil(t, schedules[0].LastFiredAt(), "failed delivery must remain due for retry")
}

func TestExecutor_FreshCronFires(t *testing.T) {
	schedStore, daemonStore, sessStore := newTestDB(t)
	sender := &mockSender{}

	pid, err := daemonStore.GetOrCreateProject(context.Background(), "/tmp/test")
	require.NoError(t, err)
	rec, err := sessStore.CreateSession(context.Background(), pid, "", "", nil)
	require.NoError(t, err)

	_, err = schedStore.AddSchedule(context.Background(), rec.ID, "* * * * *", nil, "do the fresh job", true)
	require.NoError(t, err)

	executor := schedule.NewExecutor(schedStore, sender)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	executor.Start(ctx)

	time.Sleep(500 * time.Millisecond)
	executor.Stop()

	calls := sender.getCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, "fresh", calls[0].kind)
	// A fresh tick carries only the prompt — no "Schedule tick #N" header.
	assert.Equal(t, "do the fresh job", calls[0].input)
	assert.NotContains(t, calls[0].input, "Schedule tick")
}

func TestExecutor_CronDoesNotDoubleFire(t *testing.T) {
	schedStore, daemonStore, sessStore := newTestDB(t)
	sender := &mockSender{}

	pid, err := daemonStore.GetOrCreateProject(context.Background(), "/tmp/test")
	require.NoError(t, err)
	rec, err := sessStore.CreateSession(context.Background(), pid, "", "", nil)
	require.NoError(t, err)

	sched, err := schedStore.AddSchedule(context.Background(), rec.ID, "* * * * *", nil, "periodic", false)
	require.NoError(t, err)

	now := time.Now()
	err = schedStore.UpdateScheduleLastFired(context.Background(), sched.ID(), now)
	require.NoError(t, err)

	executor := schedule.NewExecutor(schedStore, sender)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	executor.Start(ctx)

	time.Sleep(500 * time.Millisecond)
	executor.Stop()

	calls := sender.getCalls()
	assert.Empty(t, calls)
}

func TestExecutor_StopIsClean(t *testing.T) {
	schedStore, _, _ := newTestDB(t)
	sender := &mockSender{}

	executor := schedule.NewExecutor(schedStore, sender)
	ctx := context.Background()
	executor.Start(ctx)
	executor.Stop()

	// Can be called again safely
	executor.Stop()
}
