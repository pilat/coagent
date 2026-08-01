package schedule_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/tool"
)

func sleepContext() context.Context {
	return tool.WithCallID(context.Background(), "sleep-call-1")
}

func TestSleepTool_NoStore(t *testing.T) {
	tl := schedule.NewSleepTool(nil, 0)
	params, _ := json.Marshal(tool.SleepParams{Duration: "5s"})
	_, err := tl.Execute(context.Background(), params)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not available")
}

func TestSleepTool_DaemonMode(t *testing.T) {
	t.Run("creates schedule and returns ErrSuspend", func(t *testing.T) {
		store := &mockScheduleStore{nextID: 1}
		tl := schedule.NewSleepTool(store, int64(1))

		params, _ := json.Marshal(tool.SleepParams{Duration: "2h"})
		start := time.Now()
		result, err := tl.Execute(sleepContext(), params)
		elapsed := time.Since(start)

		require.ErrorIs(t, err, tool.ErrSuspend)
		assert.Nil(t, result)
		assert.Less(t, elapsed, 100*time.Millisecond, "daemon mode should not block, took: %v", elapsed)

		require.Len(t, store.schedules, 1)
		assert.NotNil(t, store.schedules[0].oneShotAt)
		assert.Contains(t, store.schedules[0].inputMsg, "Sleep completed")
		assert.Equal(t, "sleep-call-1", store.lastCallID)
	})

	t.Run("long duration not capped", func(t *testing.T) {
		store := &mockScheduleStore{nextID: 2}
		tl := schedule.NewSleepTool(store, int64(1))

		params, _ := json.Marshal(tool.SleepParams{Duration: "8h"})
		result, err := tl.Execute(sleepContext(), params)

		require.ErrorIs(t, err, tool.ErrSuspend)
		assert.Nil(t, result)
		require.Len(t, store.schedules, 1)
		wakeAt := store.schedules[0].oneShotAt
		assert.True(t, wakeAt.After(time.Now().Add(7*time.Hour)), "should be ~8h, not capped")
	})

	t.Run("days duration", func(t *testing.T) {
		store := &mockScheduleStore{nextID: 3}
		tl := schedule.NewSleepTool(store, int64(1))

		params, _ := json.Marshal(tool.SleepParams{Duration: "3d"})
		result, err := tl.Execute(sleepContext(), params)

		require.ErrorIs(t, err, tool.ErrSuspend)
		assert.Nil(t, result)
		require.Len(t, store.schedules, 1)
		wakeAt := store.schedules[0].oneShotAt
		assert.True(t, wakeAt.After(time.Now().Add(2*24*time.Hour)), "should be ~3 days")
	})

	t.Run("store error propagates", func(t *testing.T) {
		store := &mockScheduleStore{addErr: assert.AnError}
		tl := schedule.NewSleepTool(store, int64(1))

		params, _ := json.Marshal(tool.SleepParams{Duration: "1h"})
		_, err := tl.Execute(sleepContext(), params)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "store sleep schedule")
	})

	t.Run("missing tool call id cannot create an unresumable sleep", func(t *testing.T) {
		store := &mockScheduleStore{}
		tl := schedule.NewSleepTool(store, int64(1))

		params, _ := json.Marshal(tool.SleepParams{Duration: "1h"})
		_, err := tl.Execute(context.Background(), params)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "tool call id")
		assert.Empty(t, store.schedules)
	})

	t.Run("empty duration", func(t *testing.T) {
		store := &mockScheduleStore{}
		tl := schedule.NewSleepTool(store, int64(1))
		params, _ := json.Marshal(tool.SleepParams{Duration: ""})
		_, err := tl.Execute(context.Background(), params)
		assert.Error(t, err)
	})

	t.Run("invalid duration", func(t *testing.T) {
		store := &mockScheduleStore{}
		tl := schedule.NewSleepTool(store, int64(1))
		params, _ := json.Marshal(tool.SleepParams{Duration: "notaduration"})
		_, err := tl.Execute(context.Background(), params)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid duration")
	})

	t.Run("negative duration", func(t *testing.T) {
		store := &mockScheduleStore{}
		tl := schedule.NewSleepTool(store, int64(1))
		params, _ := json.Marshal(tool.SleepParams{Duration: "-5s"})
		_, err := tl.Execute(context.Background(), params)
		assert.Error(t, err)
	})

	t.Run("invalid json", func(t *testing.T) {
		store := &mockScheduleStore{}
		tl := schedule.NewSleepTool(store, int64(1))
		_, err := tl.Execute(context.Background(), json.RawMessage(`{invalid`))
		assert.Error(t, err)
	})
}

func TestSleepTool_RFC3339(t *testing.T) {
	t.Run("future timestamp", func(t *testing.T) {
		store := &mockScheduleStore{nextID: 4}
		tl := schedule.NewSleepTool(store, int64(1))

		future := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
		params, _ := json.Marshal(tool.SleepParams{Duration: future})

		result, err := tl.Execute(sleepContext(), params)
		require.ErrorIs(t, err, tool.ErrSuspend)
		assert.Nil(t, result)
		require.Len(t, store.schedules, 1)
	})

	t.Run("past timestamp", func(t *testing.T) {
		store := &mockScheduleStore{}
		tl := schedule.NewSleepTool(store, int64(1))
		past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
		params, _ := json.Marshal(tool.SleepParams{Duration: past})

		_, err := tl.Execute(context.Background(), params)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "positive")
	})
}
