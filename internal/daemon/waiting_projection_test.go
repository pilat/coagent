package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/sessionevent"
)

func TestPublishWaiting_ProjectsOnlyOneShotsOwnedByPendingSleepCalls(t *testing.T) {
	ctx := context.Background()
	mgr, _, projects, schedules := newTestManagerWithSchedule(t)
	projectID := testProject(t, projects, "/tmp/test")
	rec, err := mgr.sessionStore.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)

	now := time.Now().UTC().Truncate(time.Second)
	standaloneAt := now.Add(10 * time.Minute)
	_, err = schedules.AddSchedule(ctx, rec.ID, "", &standaloneAt, "standalone reminder", false)
	require.NoError(t, err)

	sleepAt := now.Add(20 * time.Minute)
	_, err = schedule.NewService(schedules).AddSleep(ctx, rec.ID, "sleep-call", sleepAt, "wake")
	require.NoError(t, err)

	var notifications []sessionevent.Notification
	mgr.publishWaiting(ctx, rec.ID, func(notification sessionevent.Notification) {
		notifications = append(notifications, notification)
	})

	require.Len(t, notifications, 1)
	require.Len(t, notifications[0].Waiting, 1,
		"a standalone one-shot schedule is future input, not a pending wait")
	wait := notifications[0].Waiting[0]
	assert.Equal(t, sessionevent.WaitSleep, wait.Kind)
	require.NotNil(t, wait.WakeAt)
	assert.True(t, wait.WakeAt.Equal(sleepAt))
}
