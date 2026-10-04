package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/transcript"
)

func TestPublishWaiting_ProjectsOnlyOneShotsOwnedByPendingSleepCalls(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: (&mockFactory{}).client})
	mgr, projects, schedules := h.mgr, h.store, h.schedules
	projectID := testProject(t, projects, "/tmp/test")
	rec, err := mgr.store.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Second)
	standaloneAt := now.Add(10 * time.Minute)
	_, err = schedules.AddSchedule(ctx, rec.ID, "", &standaloneAt, "standalone reminder", false)
	require.NoError(t, err)
	sleepAt := now.Add(20 * time.Minute)
	_, err = mgr.store.Commit(ctx, sessionstore.Commit{
		SessionID: rec.ID,
		Messages: []*transcript.Message{
			{Role: "assistant", ToolCalls: []byte(`[{"ID":"sleep-call","Name":"sleep","Arguments":"e30="}]`)},
		},
	})
	require.NoError(t, err)
	_, err = schedule.NewService(schedules, mgr.store.(*sessionstore.Store)).
		AddSleep(
			ctx, rec.ID, "sleep-call", sleepAt, "wake",
		)
	require.NoError(t, err)
	notifications := mgr.bus.Subscribe(rec.ID)
	defer mgr.bus.Unsubscribe(rec.ID, notifications)
	mgr.publishWaiting(ctx, rec.ID)
	var notification sessionevent.Notification
	select {
	case notification = <-notifications:
	case <-time.After(time.Second):
		t.Fatal("waiting projection was not published")
	}
	assert.Empty(t, notifications, "waiting projection must publish exactly once")
	require.Len(t, notification.Waiting, 1, "a standalone one-shot schedule is future input, not a pending wait")
	wait := notification.Waiting[0]
	assert.Equal(t, sessionevent.WaitSleep, wait.Kind)
	require.NotNil(t, wait.WakeAt)
	assert.True(t, wait.WakeAt.Equal(sleepAt))
}
