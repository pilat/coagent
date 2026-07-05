package schedule

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/sessionevent"
)

type cronAckFailStore struct {
	Store
	due *Schedule
}

func (s *cronAckFailStore) ListDueCronSchedules(_ context.Context, _ time.Time) ([]*Schedule, error) {
	return []*Schedule{s.due}, nil
}

func (*cronAckFailStore) UpdateScheduleLastFired(context.Context, int64, time.Time) error {
	return errors.New("forced cron acknowledgement failure")
}

type idempotentScheduleSender struct {
	payloadByID   map[string]string
	deliveries    []string
	notifications int
}

func (*idempotentScheduleSender) DeliverPendingCallResult(
	context.Context, int64, string, string, string,
) (bool, error) {
	return false, errors.New("unexpected pending result")
}

func (s *idempotentScheduleSender) DeliverScheduleTick(
	_ context.Context,
	_ int64,
	deliveryID, content string,
) (bool, error) {
	s.deliveries = append(s.deliveries, deliveryID)
	if previous, ok := s.payloadByID[deliveryID]; ok {
		if previous != content {
			return false, errors.New("same delivery id carried different content")
		}

		return false, nil
	}

	s.payloadByID[deliveryID] = content

	return true, nil
}

func (*idempotentScheduleSender) DeliverFreshSchedule(
	context.Context, int64, string, string,
) (bool, error) {
	return false, errors.New("unexpected fresh schedule")
}

func (s *idempotentScheduleSender) NotifySession(_ int64, _ sessionevent.Notification) {
	s.notifications++
}

func TestExecutor_CronAckRetryKeepsCanonicalIdentityAndPayload(t *testing.T) {
	firstTick := time.Date(2026, time.August, 14, 12, 0, 5, 0, time.UTC)
	secondTick := firstTick.Add(40 * time.Second)
	store := &cronAckFailStore{due: &Schedule{
		id: 11, sessionID: 3, cronExpr: "* * * * *", inputMessage: "canonical retry",
	}}
	sender := &idempotentScheduleSender{payloadByID: make(map[string]string)}
	executor := NewExecutor(store, sender).(*executor)

	require.NoError(t, executor.fireCronSchedules(context.Background(), firstTick, zap.NewNop()))
	require.NoError(t, executor.fireCronSchedules(context.Background(), secondTick, zap.NewNop()))

	require.Len(t, sender.deliveries, 2, "failed ack must retry the same occurrence")
	assert.Equal(t, sender.deliveries[0], sender.deliveries[1])
	assert.Equal(t, "schedule:cron:11:20260814T1200Z", sender.deliveries[0])
	assert.Equal(t, 1, sender.notifications, "duplicate acceptance must not republish the occurrence")
	assert.Contains(t, sender.payloadByID[sender.deliveries[0]], "2026-08-14T12:00:00Z")
}
