package schedule_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/schedule"
)

func TestService_CreationVariantsKeepExactIdentity(t *testing.T) {
	ctx := context.Background()
	schedStore, daemonStore, sessStore := newTestDB(t)
	projectID := testProject(t, daemonStore, "/tmp/project")
	rec, err := sessStore.CreateSession(ctx, projectID, "", "", nil)
	require.NoError(t, err)

	svc := schedule.NewService(schedStore)

	_, err = svc.AddRecurring(ctx, rec.ID, "", "invalid", false)
	require.Error(t, err)
	_, err = svc.AddSleep(ctx, rec.ID, "", time.Now().Add(time.Hour), "invalid")
	require.Error(t, err)

	schedules, err := svc.ListSchedules(ctx, rec.ID)
	require.NoError(t, err)
	assert.Empty(t, schedules, "invalid variants must not reach persistence")

	_, err = svc.AddRecurring(ctx, rec.ID, "0 9 * * *", "daily", false)
	require.NoError(t, err)
	_, err = svc.AddSleep(ctx, rec.ID, "sleep-call-7", time.Now().Add(time.Hour), "wake")
	require.NoError(t, err)

	pending, err := svc.PendingSleeps(ctx, rec.ID)
	require.NoError(t, err)
	require.Len(t, pending, 1, "recurring rows never masquerade as pending calls")
	assert.Equal(t, "sleep-call-7", pending[0].CallID)
}

func TestService_CancelPendingSleepsPreservesStandaloneInput(t *testing.T) {
	ctx := context.Background()
	schedStore, daemonStore, sessStore := newTestDB(t)
	projectID := testProject(t, daemonStore, "/tmp/project")
	rec, err := sessStore.CreateSession(ctx, projectID, "", "", nil)
	require.NoError(t, err)

	now := time.Now().UTC()
	standaloneAt := now.Add(time.Hour)
	_, err = schedStore.AddSchedule(ctx, rec.ID, "", &standaloneAt, "standalone input", false)
	require.NoError(t, err)

	svc := schedule.NewService(schedStore)
	_, err = svc.AddSleep(ctx, rec.ID, "sleep-call", now.Add(2*time.Hour), "wake")
	require.NoError(t, err)

	removed, err := svc.CancelPendingSleeps(ctx, rec.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), removed, "only the row owning a pending sleep call is cancelled")

	remaining, err := svc.ListSchedules(ctx, rec.ID)
	require.NoError(t, err)
	require.Len(t, remaining, 1)
	assert.Equal(t, "standalone input", remaining[0].InputMessage())
}
