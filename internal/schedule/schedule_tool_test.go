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
)

// mockScheduleStore implements schedule.Service for testing.
type mockScheduleStore struct {
	schedules    []mockScheduleEntry
	addErr       error
	removeErr    error
	nextID       int64
	lastFresh    bool
	lastInputMsg string
	lastCallID   string
}

type mockScheduleEntry struct {
	id          int64
	cronExpr    string
	oneShotAt   *time.Time
	inputMsg    string
	lastFiredAt *time.Time
	fresh       bool
}

func (e *mockScheduleEntry) ID() int64               { return e.id }
func (e *mockScheduleEntry) CronExpr() string        { return e.cronExpr }
func (e *mockScheduleEntry) OneShotAt() *time.Time   { return e.oneShotAt }
func (e *mockScheduleEntry) InputMessage() string    { return e.inputMsg }
func (e *mockScheduleEntry) LastFiredAt() *time.Time { return e.lastFiredAt }
func (e *mockScheduleEntry) Fresh() bool             { return e.fresh }

func (s *mockScheduleStore) ListSchedules(_ context.Context, _ int64) ([]schedule.Entry, error) {
	result := make([]schedule.Entry, len(s.schedules))
	for i := range s.schedules {
		result[i] = &s.schedules[i]
	}
	return result, nil
}

func (s *mockScheduleStore) PendingSleeps(_ context.Context, _ int64) ([]schedule.PendingSleep, error) {
	if s.lastCallID == "" {
		return nil, nil
	}

	return []schedule.PendingSleep{{CallID: s.lastCallID}}, nil
}

func (s *mockScheduleStore) RemoveSchedule(_ context.Context, _ int64, scheduleID int64) error {
	if s.removeErr != nil {
		return s.removeErr
	}
	for i, e := range s.schedules {
		if e.id == scheduleID {
			s.schedules = append(s.schedules[:i], s.schedules[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("schedule %d not found", scheduleID)
}

func (s *mockScheduleStore) AddRecurring(
	_ context.Context,
	_ int64,
	cronExpr string,
	inputMessage string,
	fresh bool,
) (schedule.Created, error) {
	if s.addErr != nil {
		return schedule.Created{}, s.addErr
	}
	s.lastFresh = fresh
	s.lastInputMsg = inputMessage
	id := s.nextID
	if id == 0 {
		id = 1
	}
	entry := mockScheduleEntry{id: id, cronExpr: cronExpr, inputMsg: inputMessage}
	s.schedules = append(s.schedules, entry)

	created := schedule.Created{ID: id, Type: "cron", NextFire: time.Now().Add(time.Hour).UTC()}
	return created, nil
}

func (s *mockScheduleStore) AddSleep(
	_ context.Context,
	_ int64,
	callID string,
	wakeAt time.Time,
	result string,
) (schedule.Created, error) {
	if s.addErr != nil {
		return schedule.Created{}, s.addErr
	}

	s.lastCallID = callID
	id := s.nextID
	if id == 0 {
		id = 1
	}

	s.schedules = append(s.schedules, mockScheduleEntry{
		id:        id,
		oneShotAt: &wakeAt,
		inputMsg:  result,
	})

	return schedule.Created{ID: id, Type: "interval", NextFire: wakeAt.UTC()}, nil
}

func (s *mockScheduleStore) CancelPendingSleeps(_ context.Context, _ int64) (int64, error) {
	return 0, nil
}

func (s *mockScheduleStore) RemoveAllForSession(_ context.Context, _ int64) error {
	return nil
}

func TestScheduleTool_Create_Cron(t *testing.T) {
	store := &mockScheduleStore{nextID: 2}
	tl := schedule.NewScheduleTool(int64(1), store, nil)

	params, _ := json.Marshal(map[string]string{"action": "create", "cron": "0 9 * * *"})
	result, err := tl.Execute(context.Background(), params)
	require.NoError(t, err)
	assert.Contains(t, result.Output, "cron")
	assert.Equal(t, "CRON_TZ=UTC 0 9 * * *", store.schedules[0].cronExpr)
}

func TestScheduleTool_Create_Fresh(t *testing.T) {
	store := &mockScheduleStore{nextID: 3}
	tl := schedule.NewScheduleTool(int64(1), store, nil)

	params, _ := json.Marshal(map[string]any{
		"action": "create", "cron": "0 9 * * *", "fresh": true, "prompt": "check CI and fix flakes",
	})
	_, err := tl.Execute(context.Background(), params)
	require.NoError(t, err)
	assert.True(t, store.lastFresh)
	assert.Equal(t, "check CI and fix flakes", store.lastInputMsg)
}

func TestScheduleTool_Create_FreshRequiresPrompt(t *testing.T) {
	store := &mockScheduleStore{nextID: 3}
	tl := schedule.NewScheduleTool(int64(1), store, nil)

	params, _ := json.Marshal(map[string]any{"action": "create", "cron": "0 9 * * *", "fresh": true})
	_, err := tl.Execute(context.Background(), params)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fresh schedule requires a prompt")
	assert.Empty(t, store.schedules)
}

func TestScheduleTool_Create_NotFreshByDefault(t *testing.T) {
	store := &mockScheduleStore{nextID: 3}
	tl := schedule.NewScheduleTool(int64(1), store, nil)

	params, _ := json.Marshal(map[string]string{"action": "create", "cron": "0 9 * * *"})
	_, err := tl.Execute(context.Background(), params)
	require.NoError(t, err)
	assert.False(t, store.lastFresh)
}

func TestScheduleTool_Create_InvalidCron(t *testing.T) {
	store := &mockScheduleStore{}
	tl := schedule.NewScheduleTool(int64(1), store, nil)

	params, _ := json.Marshal(map[string]string{"action": "create", "cron": "not-valid"})
	_, err := tl.Execute(context.Background(), params)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid cron expression")
}

func TestScheduleTool_Create_MissingCron(t *testing.T) {
	store := &mockScheduleStore{}
	tl := schedule.NewScheduleTool(int64(1), store, nil)

	params, _ := json.Marshal(map[string]string{"action": "create"})
	_, err := tl.Execute(context.Background(), params)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cron expression is required")
}

func TestScheduleTool_List_Empty(t *testing.T) {
	store := &mockScheduleStore{}
	tl := schedule.NewScheduleTool(int64(1), store, nil)

	params, _ := json.Marshal(map[string]string{"action": "list"})
	result, err := tl.Execute(context.Background(), params)
	require.NoError(t, err)
	assert.Contains(t, result.Output, "No active schedules")
}

func TestScheduleTool_List_WithEntries(t *testing.T) {
	fireAt := time.Now().Add(time.Hour).UTC()
	store := &mockScheduleStore{
		schedules: []mockScheduleEntry{
			{id: 1, cronExpr: "0 9 * * *"},
			{id: 2, oneShotAt: &fireAt},
		},
	}
	tl := schedule.NewScheduleTool(int64(1), store, nil)

	params, _ := json.Marshal(map[string]string{"action": "list"})
	result, err := tl.Execute(context.Background(), params)
	require.NoError(t, err)
	assert.Contains(t, result.Output, "id=1")
	assert.Contains(t, result.Output, "id=2")
	assert.Contains(t, result.Output, "cron=")
	assert.Contains(t, result.Output, "fires_at=")
}

func TestScheduleTool_Cancel(t *testing.T) {
	store := &mockScheduleStore{
		schedules: []mockScheduleEntry{{id: 1, cronExpr: "0 9 * * *"}},
	}
	tl := schedule.NewScheduleTool(int64(1), store, nil)

	params, _ := json.Marshal(map[string]any{"action": "cancel", "id": 1})
	result, err := tl.Execute(context.Background(), params)
	require.NoError(t, err)
	assert.Contains(t, result.Output, "cancelled")
	assert.Empty(t, store.schedules)
}

func TestScheduleTool_Cancel_MissingID(t *testing.T) {
	store := &mockScheduleStore{}
	tl := schedule.NewScheduleTool(int64(1), store, nil)

	params, _ := json.Marshal(map[string]string{"action": "cancel"})
	_, err := tl.Execute(context.Background(), params)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "id is required")
}

func TestScheduleTool_Cancel_NotFound(t *testing.T) {
	store := &mockScheduleStore{}
	tl := schedule.NewScheduleTool(int64(1), store, nil)

	params, _ := json.Marshal(map[string]any{"action": "cancel", "id": 99999})
	_, err := tl.Execute(context.Background(), params)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestScheduleTool_UnknownAction(t *testing.T) {
	store := &mockScheduleStore{}
	tl := schedule.NewScheduleTool(int64(1), store, nil)

	params, _ := json.Marshal(map[string]string{"action": "restart"})
	_, err := tl.Execute(context.Background(), params)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown action")
}

func TestScheduleTool_NoDaemon(t *testing.T) {
	tl := schedule.NewScheduleTool(int64(1), nil, nil)

	params, _ := json.Marshal(map[string]string{"action": "list"})
	_, err := tl.Execute(context.Background(), params)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not available")
}
