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

// capFakeStore embeds Store (nil) and overrides only the two methods
// fireOneShotSchedules touches; any other call would panic, which is the point.
type capFakeStore struct {
	Store
	due     []*Schedule
	removed map[int64]bool
}

func (f *capFakeStore) ListDueSchedules(_ context.Context, _ time.Time) ([]*Schedule, error) {
	var out []*Schedule

	for _, s := range f.due {
		if !f.removed[s.id] {
			out = append(out, s)
		}
	}

	return out, nil
}

func (f *capFakeStore) RemoveSchedule(_ context.Context, id int64) error {
	f.removed[id] = true
	return nil
}

type capFailSender struct{ calls int }

func (s *capFailSender) DeliverPendingCallResult(
	_ context.Context, _ int64, _, _, _ string,
) (bool, error) {
	s.calls++
	return false, errors.New("session gone")
}

func (s *capFailSender) DeliverScheduleTick(_ context.Context, _ int64, _, _ string) (bool, error) {
	return true, nil
}

func (s *capFailSender) DeliverFreshSchedule(_ context.Context, _ int64, _, _ string) (bool, error) {
	return true, nil
}

func (s *capFailSender) NotifySession(_ int64, _ sessionevent.Notification) {}

// TestExecutor_OneShotDroppedAfterMaxAttempts pins the leak fix: a one-shot whose
// delivery keeps failing is retried, then dropped after the cap — never forever.
func TestExecutor_OneShotDroppedAfterMaxAttempts(t *testing.T) {
	past := time.Now().Add(-time.Minute)
	sched := &Schedule{
		id: 7, sessionID: 3, oneShotAt: &past, inputMessage: "wake",
		metadata: ScheduleMetadata{ToolCallID: "sleep-call"},
	}
	store := &capFakeStore{due: []*Schedule{sched}, removed: map[int64]bool{}}
	sender := &capFailSender{}
	e := NewExecutor(store, sender).(*executor)

	log := zap.NewNop()
	ctx := context.Background()

	for i := 1; i < maxOneShotAttempts; i++ {
		require.NoError(t, e.fireOneShotSchedules(ctx, time.Now(), log))
		require.Falsef(t, store.removed[sched.id], "dropped too early at attempt %d", i)
	}

	require.NoError(t, e.fireOneShotSchedules(ctx, time.Now(), log))
	assert.Truef(t, store.removed[sched.id], "should be dropped after %d attempts", maxOneShotAttempts)
	assert.Equal(t, maxOneShotAttempts, sender.calls)
}
