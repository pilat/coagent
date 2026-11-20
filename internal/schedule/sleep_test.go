package schedule_test

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestSleepTool_InTurnUpToTheLimit(t *testing.T) {
	t.Run("short sleep returns its result inside the call", func(t *testing.T) {
		store := &mockScheduleStore{}
		params, err := json.Marshal(tool.SleepParams{Duration: "20ms"})
		require.NoError(t, err)
		start := time.Now()
		result, err := schedule.NewSleepTool(store, 1).Execute(sleepContext(), params)
		elapsed := time.Since(start)

		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Equal(t, "Sleep completed (20ms).", result.Output)
		assert.GreaterOrEqual(t, elapsed, 20*time.Millisecond)
		assert.Empty(t, store.schedules)
	})

	t.Run("the limit itself waits in turn", func(t *testing.T) {
		store := &mockScheduleStore{}
		ctx, cancel := context.WithTimeout(sleepContext(), 50*time.Millisecond)
		defer cancel()
		params, err := json.Marshal(tool.SleepParams{Duration: tool.InTurnWaitLimit.String()})
		require.NoError(t, err)
		start := time.Now()
		result, err := schedule.NewSleepTool(store, 1).Execute(ctx, params)
		elapsed := time.Since(start)

		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Nil(t, result)
		assert.Empty(t, store.schedules)
		assert.Less(t, elapsed, time.Second)
	})

	t.Run("a timestamp within the limit waits in turn", func(t *testing.T) {
		store := &mockScheduleStore{}
		ctx, cancel := context.WithTimeout(sleepContext(), 50*time.Millisecond)
		defer cancel()
		duration := time.Now().Add(3 * time.Second).UTC().Format(time.RFC3339)
		params, err := json.Marshal(tool.SleepParams{Duration: duration})
		require.NoError(t, err)
		_, err = schedule.NewSleepTool(store, 1).Execute(ctx, params)

		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Empty(t, store.schedules)
	})

	t.Run("just over the limit is durable", func(t *testing.T) {
		for _, duration := range []string{
			(tool.InTurnWaitLimit + time.Millisecond).String(),
			(tool.InTurnWaitLimit + 500*time.Millisecond).String(),
			fmt.Sprintf("%dms", (tool.InTurnWaitLimit + 500*time.Millisecond).Milliseconds()),
		} {
			t.Run(duration, func(t *testing.T) {
				store := &mockScheduleStore{}
				params, err := json.Marshal(tool.SleepParams{Duration: duration})
				require.NoError(t, err)
				start := time.Now()
				result, err := schedule.NewSleepTool(store, 1).Execute(sleepContext(), params)
				elapsed := time.Since(start)

				require.ErrorIs(t, err, tool.ErrSuspend)
				assert.Nil(t, result)
				require.Len(t, store.schedules, 1)
				assert.Contains(t, store.schedules[0].inputMsg, "Sleep completed ("+duration+")")
				assert.Less(t, elapsed, 100*time.Millisecond)
			})
		}
	})
}

func TestSleepTool_DescriptionNamesTheInTurnLimit(t *testing.T) {
	assert.Contains(
		t,
		schedule.NewSleepTool(nil, 0).Description(),
		"Up to "+tool.InTurnWaitLimit.String()+" the sleep happens inside the current turn; a longer one suspends the session until the timer fires.",
	)
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
