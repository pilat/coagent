package schedule_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/transcript"
)

type mockSender struct {
	mu            sync.Mutex
	notifications []sessionevent.Notification
}

func (m *mockSender) NotifySession(_ int64, notification sessionevent.Notification) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notifications = append(m.notifications, notification)
}

func (m *mockSender) getNotifications() []sessionevent.Notification {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]sessionevent.Notification{}, m.notifications...)
}

func newTestDB(t *testing.T) (schedule.Store, *sessionstore.Store, *sessionstore.Store) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := migrate.OpenDB(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	provider, err := migrate.NewProvider(db)
	require.NoError(t, err)
	_, err = provider.Up(context.Background())
	require.NoError(t, err)
	sessions := sessionstore.NewStore(db)
	return schedule.NewStore(db, sessions), sessions, sessions
}

func TestExecutor_OneShotFires(t *testing.T) {
	schedStore, daemonStore, sessStore := newTestDB(t)
	sender := &mockSender{}

	pid, err := daemonStore.GetOrCreateProject(context.Background(), "/tmp/test")
	require.NoError(t, err)
	rec, err := sessStore.CreateSession(context.Background(), pid, "", "", nil)
	require.NoError(t, err)

	_, err = sessStore.Commit(
		t.Context(),
		sessionstore.Commit{
			SessionID: rec.ID,
			Messages: []*transcript.Message{
				{Role: "assistant", ToolCalls: []byte(`[{"ID":"sleep-call-1","Name":"sleep","Arguments":"e30="}]`)},
			},
		},
	)
	require.NoError(t, err)

	past := time.Now().Add(-time.Minute).UTC()
	_, err = schedStore.AddSchedule(
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

	calls, err := sessStore.ListPending(t.Context(), rec.ID)
	require.NoError(t, err)
	require.Len(t, calls, 1)
	assert.Equal(t, rec.ID, calls[0].SessionID)
	assert.Equal(t, sessionstore.InputSourceCallResult, calls[0].Source)
	assert.Equal(t, "sleep-call-1", calls[0].Attributes["call_id"])
	assert.Equal(t, tool.IDSleep, calls[0].Attributes["tool_id"])
	assert.Equal(t, "wake up and check", calls[0].RawContent)

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
	_, err = schedStore.AddSchedule(
		context.Background(),
		rec.ID,
		"",
		&past,
		"one-time check",
		false,
		schedule.ScheduleMetadata{},
	)
	require.NoError(t, err)

	executor := schedule.NewExecutor(schedStore, sender)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	executor.Start(ctx)
	time.Sleep(500 * time.Millisecond)
	executor.Stop()

	calls, err := sessStore.ListPending(t.Context(), rec.ID)
	require.NoError(t, err)
	require.Len(t, calls, 1)
	assert.Equal(t, sessionstore.InputSourceSchedule, calls[0].Source)
	assert.Equal(t, false, calls[0].Attributes["fresh"])
	assert.Equal(t, "one-time check", calls[0].RawContent)

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
	_, err = schedStore.AddSchedule(
		context.Background(),
		rec.ID,
		"",
		&past,
		"fresh one-time work",
		true,
		schedule.ScheduleMetadata{},
	)
	require.NoError(t, err)

	executor := schedule.NewExecutor(schedStore, sender)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	executor.Start(ctx)
	t.Cleanup(executor.Stop)

	require.Eventually(
		t,
		func() bool { return len(sender.getNotifications()) == 1 },
		5*time.Second,
		10*time.Millisecond,
	)
	calls, err := sessStore.ListPending(t.Context(), rec.ID)
	require.NoError(t, err)
	require.Len(t, calls, 1)
	assert.Equal(t, sessionstore.InputSourceSchedule, calls[0].Source)
	assert.Equal(t, true, calls[0].Attributes["fresh"])
	assert.Equal(t, "fresh one-time work", calls[0].RawContent)
}

func TestExecutor_CronFires(t *testing.T) {
	schedStore, daemonStore, sessStore := newTestDB(t)
	sender := &mockSender{}

	pid, err := daemonStore.GetOrCreateProject(context.Background(), "/tmp/test")
	require.NoError(t, err)
	rec, err := sessStore.CreateSession(context.Background(), pid, "", "", nil)
	require.NoError(t, err)

	_, err = schedStore.AddSchedule(
		context.Background(),
		rec.ID,
		"* * * * *",
		nil,
		"periodic check",
		false,
		schedule.ScheduleMetadata{},
	)
	require.NoError(t, err)

	executor := schedule.NewExecutor(schedStore, sender)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	executor.Start(ctx)

	time.Sleep(500 * time.Millisecond)
	executor.Stop()

	calls, err := sessStore.ListPending(t.Context(), rec.ID)
	require.NoError(t, err)
	require.Len(t, calls, 1)
	assert.Equal(t, sessionstore.InputSourceSchedule, calls[0].Source)
	assert.Equal(t, false, calls[0].Attributes["fresh"])
	assert.Contains(t, calls[0].RawContent, "Schedule tick #1")
	assert.Contains(t, calls[0].RawContent, "periodic check")

	schedules, err := schedStore.ListSchedules(context.Background(), rec.ID)
	require.NoError(t, err)
	require.Len(t, schedules, 1)
	assert.NotNil(t, schedules[0].LastFiredAt(), "a delivered tick is durably acknowledged")
}

func TestExecutor_CronDeliveryFailureIsNotAcknowledged(t *testing.T) {
	schedStore, daemonStore, sessStore := newTestDB(t)
	sender := &mockSender{}

	pid, err := daemonStore.GetOrCreateProject(context.Background(), "/tmp/test")
	require.NoError(t, err)
	rec, err := sessStore.CreateSession(context.Background(), pid, "", "", nil)
	require.NoError(t, err)

	_, err = schedStore.AddSchedule(
		context.Background(),
		rec.ID,
		"* * * * *",
		nil,
		"retry me",
		false,
		schedule.ScheduleMetadata{},
	)
	require.NoError(t, err)

	require.NoError(t, sessStore.UpdateSessionStatus(t.Context(), rec.ID, sessionstore.SessionStatusKilled))
	executor := schedule.NewExecutor(schedStore, sender)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	executor.Start(ctx)
	time.Sleep(500 * time.Millisecond)
	executor.Stop()

	assert.Empty(t, sender.getNotifications())
	inputs, err := sessStore.ListPending(t.Context(), rec.ID)
	require.NoError(t, err)
	assert.Empty(t, inputs)
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

	_, err = schedStore.AddSchedule(
		context.Background(),
		rec.ID,
		"* * * * *",
		nil,
		"do the fresh job",
		true,
		schedule.ScheduleMetadata{},
	)
	require.NoError(t, err)

	executor := schedule.NewExecutor(schedStore, sender)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	executor.Start(ctx)

	time.Sleep(500 * time.Millisecond)
	executor.Stop()

	calls, err := sessStore.ListPending(t.Context(), rec.ID)
	require.NoError(t, err)
	require.Len(t, calls, 1)
	assert.Equal(t, true, calls[0].Attributes["fresh"])
	// A fresh tick carries only the prompt — no "Schedule tick #N" header.
	assert.Equal(t, "do the fresh job", calls[0].RawContent)
	assert.NotContains(t, calls[0].RawContent, "Schedule tick")
}

func TestExecutor_CronDoesNotDoubleFire(t *testing.T) {
	schedStore, daemonStore, sessStore := newTestDB(t)
	sender := &mockSender{}

	pid, err := daemonStore.GetOrCreateProject(context.Background(), "/tmp/test")
	require.NoError(t, err)
	rec, err := sessStore.CreateSession(context.Background(), pid, "", "", nil)
	require.NoError(t, err)

	sched, err := schedStore.AddSchedule(
		context.Background(),
		rec.ID,
		"* * * * *",
		nil,
		"periodic",
		false,
		schedule.ScheduleMetadata{},
	)
	require.NoError(t, err)

	now := time.Now()
	applied, err := schedStore.DeliverCron(t.Context(), sched, "periodic", nil, now.Truncate(time.Minute), now)
	require.NoError(t, err)
	require.True(t, applied)

	executor := schedule.NewExecutor(schedStore, sender)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	executor.Start(ctx)

	time.Sleep(500 * time.Millisecond)
	executor.Stop()

	assert.Empty(t, sender.getNotifications())
	inputs, err := sessStore.ListPending(t.Context(), rec.ID)
	require.NoError(t, err)
	assert.Len(t, inputs, 1)
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
