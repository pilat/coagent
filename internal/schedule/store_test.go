package schedule_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/daemon"
)

func testProject(t *testing.T, ds daemon.Store, workDir string) int64 {
	t.Helper()
	pid, err := ds.GetOrCreateProject(context.Background(), workDir)
	require.NoError(t, err)
	return pid
}

func TestStore_AddSchedule_OneShot(t *testing.T) {
	ss, ds, sessStore := newTestDB(t)
	pid := testProject(t, ds, "/tmp/project")

	rec, err := sessStore.CreateSession(context.Background(), pid, "", "", nil)
	require.NoError(t, err)

	oneShot := time.Now().Add(2 * time.Hour).UTC()
	sched, err := ss.AddSchedule(context.Background(), rec.ID, "", &oneShot, "wake up and check", false)
	require.NoError(t, err)
	assert.NotEmpty(t, sched.ID())
	assert.Equal(t, rec.ID, sched.SessionID())
	assert.Empty(t, sched.CronExpr())
	assert.NotNil(t, sched.OneShotAt())
	assert.Equal(t, "wake up and check", sched.InputMessage())
}

func TestStore_AddSchedule_Cron(t *testing.T) {
	ss, ds, sessStore := newTestDB(t)
	pid := testProject(t, ds, "/tmp/project")

	rec, err := sessStore.CreateSession(context.Background(), pid, "", "", nil)
	require.NoError(t, err)

	sched, err := ss.AddSchedule(context.Background(), rec.ID, "0 9 * * *", nil, "daily check", false)
	require.NoError(t, err)
	assert.Equal(t, "0 9 * * *", sched.CronExpr())
	assert.Nil(t, sched.OneShotAt())
}

func TestStore_AddSchedule_FreshRoundTrips(t *testing.T) {
	ss, ds, sessStore := newTestDB(t)
	pid := testProject(t, ds, "/tmp/project")

	rec, err := sessStore.CreateSession(context.Background(), pid, "", "", nil)
	require.NoError(t, err)

	sched, err := ss.AddSchedule(context.Background(), rec.ID, "0 9 * * *", nil, "fresh job", true)
	require.NoError(t, err)
	assert.True(t, sched.Fresh())

	// Reload from the DB (scan path) to confirm the column persisted.
	list, err := ss.ListSchedules(context.Background(), rec.ID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.True(t, list[0].Fresh())
}

func TestStore_AddSchedule_DefaultsNotFresh(t *testing.T) {
	ss, ds, sessStore := newTestDB(t)
	pid := testProject(t, ds, "/tmp/project")

	rec, err := sessStore.CreateSession(context.Background(), pid, "", "", nil)
	require.NoError(t, err)

	_, err = ss.AddSchedule(context.Background(), rec.ID, "0 9 * * *", nil, "plain", false)
	require.NoError(t, err)

	list, err := ss.ListSchedules(context.Background(), rec.ID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.False(t, list[0].Fresh())
}

func TestStore_AddSchedule_NeitherCronNorOneShot(t *testing.T) {
	ss, ds, sessStore := newTestDB(t)
	pid := testProject(t, ds, "/tmp/project")

	rec, err := sessStore.CreateSession(context.Background(), pid, "", "", nil)
	require.NoError(t, err)

	_, err = ss.AddSchedule(context.Background(), rec.ID, "", nil, "invalid", false)
	assert.Error(t, err)
}

func TestStore_RemoveSchedule(t *testing.T) {
	ss, ds, sessStore := newTestDB(t)
	pid := testProject(t, ds, "/tmp/project")

	rec, err := sessStore.CreateSession(context.Background(), pid, "", "", nil)
	require.NoError(t, err)

	oneShot := time.Now().Add(time.Hour).UTC()
	sched, err := ss.AddSchedule(context.Background(), rec.ID, "", &oneShot, "msg", false)
	require.NoError(t, err)

	err = ss.RemoveSchedule(context.Background(), sched.ID())
	require.NoError(t, err)

	schedules, err := ss.ListSchedules(context.Background(), rec.ID)
	require.NoError(t, err)
	assert.Empty(t, schedules)
}

func TestStore_RemoveSchedule_NotFound(t *testing.T) {
	ss, _, _ := newTestDB(t)

	err := ss.RemoveSchedule(context.Background(), 99999)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestStore_RemoveScheduleForSession(t *testing.T) {
	t.Run("removes own schedule", func(t *testing.T) {
		ss, ds, sessStore := newTestDB(t)
		pid := testProject(t, ds, "/tmp/project")

		rec, err := sessStore.CreateSession(context.Background(), pid, "", "", nil)
		require.NoError(t, err)

		oneShot := time.Now().Add(time.Hour).UTC()
		sched, err := ss.AddSchedule(context.Background(), rec.ID, "", &oneShot, "msg", false)
		require.NoError(t, err)

		err = ss.RemoveScheduleForSession(context.Background(), rec.ID, sched.ID())
		require.NoError(t, err)

		schedules, err := ss.ListSchedules(context.Background(), rec.ID)
		require.NoError(t, err)
		assert.Empty(t, schedules)
	})

	t.Run("cannot remove another session's schedule", func(t *testing.T) {
		ss, ds, sessStore := newTestDB(t)
		pid1 := testProject(t, ds, "/tmp/project1")
		pid2 := testProject(t, ds, "/tmp/project2")

		rec1, err := sessStore.CreateSession(context.Background(), pid1, "", "", nil)
		require.NoError(t, err)
		rec2, err := sessStore.CreateSession(context.Background(), pid2, "", "", nil)
		require.NoError(t, err)

		oneShot := time.Now().Add(time.Hour).UTC()
		sched, err := ss.AddSchedule(context.Background(), rec1.ID, "", &oneShot, "msg", false)
		require.NoError(t, err)

		err = ss.RemoveScheduleForSession(context.Background(), rec2.ID, sched.ID())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")

		schedules, err := ss.ListSchedules(context.Background(), rec1.ID)
		require.NoError(t, err)
		assert.Len(t, schedules, 1)
	})

	t.Run("nonexistent schedule", func(t *testing.T) {
		ss, ds, sessStore := newTestDB(t)
		pid := testProject(t, ds, "/tmp/project")

		rec, err := sessStore.CreateSession(context.Background(), pid, "", "", nil)
		require.NoError(t, err)

		err = ss.RemoveScheduleForSession(context.Background(), rec.ID, 99999)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})
}

func TestStore_ListSchedules(t *testing.T) {
	ss, ds, sessStore := newTestDB(t)
	pid := testProject(t, ds, "/tmp/project")

	rec, err := sessStore.CreateSession(context.Background(), pid, "", "", nil)
	require.NoError(t, err)

	oneShot := time.Now().Add(time.Hour).UTC()
	_, err = ss.AddSchedule(context.Background(), rec.ID, "", &oneShot, "one shot msg", false)
	require.NoError(t, err)
	_, err = ss.AddSchedule(context.Background(), rec.ID, "0 9 * * *", nil, "cron msg", false)
	require.NoError(t, err)

	schedules, err := ss.ListSchedules(context.Background(), rec.ID)
	require.NoError(t, err)
	assert.Len(t, schedules, 2)
}

func TestStore_ListDueSchedules(t *testing.T) {
	ss, ds, sessStore := newTestDB(t)
	pid := testProject(t, ds, "/tmp/project")

	rec, err := sessStore.CreateSession(context.Background(), pid, "", "", nil)
	require.NoError(t, err)

	past := time.Now().Add(-time.Hour).UTC()
	future := time.Now().Add(time.Hour).UTC()

	_, err = ss.AddSchedule(context.Background(), rec.ID, "", &past, "overdue", false)
	require.NoError(t, err)
	_, err = ss.AddSchedule(context.Background(), rec.ID, "", &future, "not yet", false)
	require.NoError(t, err)

	due, err := ss.ListDueSchedules(context.Background(), time.Now().UTC())
	require.NoError(t, err)
	assert.Len(t, due, 1)
	assert.Equal(t, "overdue", due[0].InputMessage())
}

func TestStore_ListDueCronSchedules(t *testing.T) {
	ss, ds, sessStore := newTestDB(t)
	pid := testProject(t, ds, "/tmp/project")

	rec, err := sessStore.CreateSession(context.Background(), pid, "", "", nil)
	require.NoError(t, err)

	_, err = ss.AddSchedule(context.Background(), rec.ID, "0 9 * * *", nil, "daily check", false)
	require.NoError(t, err)

	oneShot := time.Now().Add(time.Hour).UTC()
	_, err = ss.AddSchedule(context.Background(), rec.ID, "", &oneShot, "one shot", false)
	require.NoError(t, err)

	crons, err := ss.ListDueCronSchedules(context.Background(), time.Now().UTC())
	require.NoError(t, err)
	assert.Len(t, crons, 1)
	assert.Equal(t, "0 9 * * *", crons[0].CronExpr())
}

func TestStore_RemoveOneShotSchedules(t *testing.T) {
	t.Run("removes one-shot schedules, preserves cron", func(t *testing.T) {
		ss, ds, sessStore := newTestDB(t)
		pid := testProject(t, ds, "/tmp/project")

		rec, err := sessStore.CreateSession(context.Background(), pid, "", "", nil)
		require.NoError(t, err)

		oneShot := time.Now().Add(time.Hour).UTC()
		_, err = ss.AddSchedule(context.Background(), rec.ID, "", &oneShot, "one-shot msg", false)
		require.NoError(t, err)
		_, err = ss.AddSchedule(context.Background(), rec.ID, "0 9 * * *", nil, "cron msg", false)
		require.NoError(t, err)

		count, err := ss.RemoveOneShotSchedules(context.Background(), rec.ID)
		require.NoError(t, err)
		assert.Equal(t, int64(1), count)

		remaining, err := ss.ListSchedules(context.Background(), rec.ID)
		require.NoError(t, err)
		require.Len(t, remaining, 1)
		assert.Equal(t, "0 9 * * *", remaining[0].CronExpr())
	})

	t.Run("returns 0 when no one-shot schedules exist", func(t *testing.T) {
		ss, ds, sessStore := newTestDB(t)
		pid := testProject(t, ds, "/tmp/project")

		rec, err := sessStore.CreateSession(context.Background(), pid, "", "", nil)
		require.NoError(t, err)
		_, err = ss.AddSchedule(context.Background(), rec.ID, "0 9 * * *", nil, "cron only", false)
		require.NoError(t, err)

		count, err := ss.RemoveOneShotSchedules(context.Background(), rec.ID)
		require.NoError(t, err)
		assert.Equal(t, int64(0), count)
	})

	t.Run("removes multiple one-shot schedules", func(t *testing.T) {
		ss, ds, sessStore := newTestDB(t)
		pid := testProject(t, ds, "/tmp/project")

		rec, err := sessStore.CreateSession(context.Background(), pid, "", "", nil)
		require.NoError(t, err)

		for i := range 3 {
			t1 := time.Now().Add(time.Duration(i+1) * time.Hour).UTC()
			_, err = ss.AddSchedule(context.Background(), rec.ID, "", &t1, "msg", false)
			require.NoError(t, err)
		}

		count, err := ss.RemoveOneShotSchedules(context.Background(), rec.ID)
		require.NoError(t, err)
		assert.Equal(t, int64(3), count)

		remaining, err := ss.ListSchedules(context.Background(), rec.ID)
		require.NoError(t, err)
		assert.Empty(t, remaining)
	})
}

func TestStore_RemoveAllSchedules(t *testing.T) {
	t.Run("removes both one-shot and cron rows for the session", func(t *testing.T) {
		ss, ds, sessStore := newTestDB(t)
		pid := testProject(t, ds, "/tmp/project")

		rec, err := sessStore.CreateSession(context.Background(), pid, "", "", nil)
		require.NoError(t, err)

		oneShot := time.Now().Add(time.Hour).UTC()
		_, err = ss.AddSchedule(context.Background(), rec.ID, "", &oneShot, "one-shot msg", false)
		require.NoError(t, err)
		_, err = ss.AddSchedule(context.Background(), rec.ID, "0 9 * * *", nil, "cron msg", false)
		require.NoError(t, err)

		require.NoError(t, ss.RemoveAllSchedules(context.Background(), rec.ID))

		remaining, err := ss.ListSchedules(context.Background(), rec.ID)
		require.NoError(t, err)
		assert.Empty(t, remaining)
	})

	t.Run("leaves other sessions' rows untouched", func(t *testing.T) {
		ss, ds, sessStore := newTestDB(t)
		pid := testProject(t, ds, "/tmp/project")

		recA, err := sessStore.CreateSession(context.Background(), pid, "", "", nil)
		require.NoError(t, err)
		recB, err := sessStore.CreateSession(context.Background(), pid, "", "", nil)
		require.NoError(t, err)

		_, err = ss.AddSchedule(context.Background(), recA.ID, "0 9 * * *", nil, "a cron", false)
		require.NoError(t, err)
		oneShotB := time.Now().Add(time.Hour).UTC()
		_, err = ss.AddSchedule(context.Background(), recB.ID, "", &oneShotB, "b one-shot", false)
		require.NoError(t, err)

		require.NoError(t, ss.RemoveAllSchedules(context.Background(), recA.ID))

		remainingA, err := ss.ListSchedules(context.Background(), recA.ID)
		require.NoError(t, err)
		assert.Empty(t, remainingA)

		remainingB, err := ss.ListSchedules(context.Background(), recB.ID)
		require.NoError(t, err)
		assert.Len(t, remainingB, 1, "other session's schedule is untouched")
	})
}

func TestStore_ScheduleAccessors(t *testing.T) {
	ss, ds, sessStore := newTestDB(t)
	pid := testProject(t, ds, "/tmp/project")

	rec, err := sessStore.CreateSession(context.Background(), pid, "", "", nil)
	require.NoError(t, err)

	oneShot := time.Now().Add(time.Hour).UTC()
	sched, err := ss.AddSchedule(context.Background(), rec.ID, "", &oneShot, "test msg", false)
	require.NoError(t, err)

	assert.NotZero(t, sched.ID())
	assert.Equal(t, rec.ID, sched.SessionID())
	assert.Empty(t, sched.CronExpr())
	assert.NotNil(t, sched.OneShotAt())
	assert.Equal(t, "test msg", sched.InputMessage())
	assert.Nil(t, sched.LastFiredAt())
}
