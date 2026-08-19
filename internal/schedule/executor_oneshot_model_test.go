package schedule

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/sessionevent"
)

type oneShotAction uint8

const (
	oneShotFail oneShotAction = iota
	oneShotAcknowledge
	oneShotRestart
	modelOneShotAttempts = 10
)

type oneShotModel struct {
	present             bool
	consecutiveFailures int
	deliveryCount       int
}

func (m *oneShotModel) step(action oneShotAction) {
	if action == oneShotRestart {
		m.consecutiveFailures = 0
		return
	}

	m.deliveryCount++
	if action == oneShotAcknowledge {
		m.present = false
		m.consecutiveFailures = 0
		return
	}

	m.consecutiveFailures++
	if m.consecutiveFailures == modelOneShotAttempts {
		m.present = false
		m.consecutiveFailures = 0
	}
}

type modelSender struct {
	actions []oneShotAction
	calls   int
}

func (s *modelSender) DeliverPendingCallResult(
	context.Context, int64, string, string, string,
) (bool, error) {
	s.calls++
	action := s.actions[0]
	s.actions = s.actions[1:]
	if action == oneShotFail {
		return false, errors.New("session unavailable")
	}

	return false, nil // a prior delivery is an acknowledged idempotent result.
}

func (*modelSender) DeliverScheduleTick(context.Context, int64, string, string) (bool, error) {
	return false, errors.New("unexpected standalone delivery")
}

func (*modelSender) DeliverFreshSchedule(context.Context, int64, string, string) (bool, error) {
	return false, errors.New("unexpected fresh delivery")
}

func (*modelSender) NotifySession(int64, sessionevent.Notification) {}

type oneShotHarness struct {
	t        *testing.T
	ctx      context.Context
	store    Store
	session  int64
	schedule int64
	sender   *modelSender
	exec     *executor
	model    oneShotModel
	now      time.Time
}

func newOneShotHarness(t *testing.T) *oneShotHarness {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "schedule-model.db")
	db, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(ctx, db, dbPath))

	sessionID := seedOneShotSession(ctx, t, db)
	store := NewStore(db)
	now := time.Date(2026, time.August, 18, 12, 0, 0, 0, time.UTC)
	due := now.Add(-time.Second)
	schedule, err := store.AddScheduleWithMeta(
		ctx, sessionID, "", &due, "wake", false, ScheduleMetadata{ToolCallID: "sleep-1"},
	)
	require.NoError(t, err)

	h := &oneShotHarness{
		t: t, ctx: ctx, store: store, session: sessionID, schedule: schedule.id, sender: &modelSender{},
		now: now, model: oneShotModel{present: true},
	}
	h.restart()

	return h
}

func seedOneShotSession(ctx context.Context, t *testing.T, db *sql.DB) int64 {
	t.Helper()
	project, err := db.ExecContext(ctx, `INSERT INTO projects (work_dir, name) VALUES (?, '')`, t.TempDir())
	require.NoError(t, err)
	projectID, err := project.LastInsertId()
	require.NoError(t, err)
	session, err := db.ExecContext(
		ctx,
		`INSERT INTO sessions (project_id, agent_type) VALUES (?, 'general')`,
		projectID,
	)
	require.NoError(t, err)
	sessionID, err := session.LastInsertId()
	require.NoError(t, err)

	return sessionID
}

func (h *oneShotHarness) restart() {
	h.exec = NewExecutor(h.store, h.sender).(*executor)
}

func (h *oneShotHarness) step(action oneShotAction) {
	h.t.Helper()
	h.model.step(action)
	if action == oneShotRestart {
		h.restart()
	} else {
		h.sender.actions = append(h.sender.actions, action)
		require.NoError(h.t, h.exec.fireOneShotSchedules(h.ctx, h.now, zap.NewNop()))
	}

	schedules, err := h.store.ListSchedules(h.ctx, h.session)
	require.NoError(h.t, err)
	assert.Equal(h.t, h.model.present, len(schedules) == 1)
	assert.Equal(h.t, h.model.consecutiveFailures, h.exec.oneShotAttempts[h.schedule])
	assert.Equal(h.t, h.model.deliveryCount, h.sender.calls)
}

func TestExecutor_OneShotBoundedDeliveryModel(t *testing.T) {
	t.Run("nine failures retain the row and the tenth drops it", func(t *testing.T) {
		t.Parallel()
		h := newOneShotHarness(t)
		for range modelOneShotAttempts - 1 {
			h.step(oneShotFail)
		}
		h.step(oneShotFail)
	})

	t.Run("replacement resets the in-memory attempt count", func(t *testing.T) {
		t.Parallel()
		h := newOneShotHarness(t)
		for range modelOneShotAttempts - 1 {
			h.step(oneShotFail)
		}
		h.step(oneShotRestart)
		for range modelOneShotAttempts - 1 {
			h.step(oneShotFail)
		}
		h.step(oneShotFail)
	})

	t.Run("acknowledged idempotent delivery removes the row and attempts", func(t *testing.T) {
		t.Parallel()
		h := newOneShotHarness(t)
		h.step(oneShotFail)
		h.step(oneShotAcknowledge)
	})
}
