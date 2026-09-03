package progressruntime

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionstore"
)

// panickingProgressStore explodes on the first reconciler call, simulating a
// store defect that must cost one tick, not the reconciler's life.
type panickingProgressStore struct {
	sessionstore.ProgressStore
	sessionstore.ReadinessStore
}

type staticProgressStore struct {
	sessionstore.ProgressStore
	sessionstore.ReadinessStore
	facts *sessionstore.ProgressFacts
}

func (s staticProgressStore) ListAutonomousProgressRoots(context.Context) ([]int64, error) {
	return []int64{s.facts.RootID}, nil
}

func (s staticProgressStore) CaptureProgress(context.Context, int64) (*sessionstore.ProgressFacts, error) {
	return s.facts, nil
}

func (panickingProgressStore) ListAutonomousProgressRoots(context.Context) ([]int64, error) {
	panic("store exploded")
}

// A panicking tick must not kill the reconciler goroutine: silence snapshots
// and duration-fire observation keep serving later deadlines.
func TestReconcileProgressSafelySurvivesStorePanic(t *testing.T) {
	t.Parallel()

	runtime := New(
		panickingProgressStore{}, nil,
		func(int64) bool { return false }, func(int64) bool { return false }, nil, nil, nil,
	)
	// The recovered panic is logged; a quiet logger keeps the test output honest.
	ctx := logger.ToContext(context.Background(), zap.NewNop())
	delay := runtime.Reconcile(ctx, time.Now().UTC())

	assert.Equal(t, SilenceInterval, delay,
		"the recovered tick must reschedule at the ordinary silence interval")
}

func TestReconcileProgressSelectsDeadlineByMainModelActivity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		working bool
		want    time.Duration
	}{
		{name: "main model working", working: true, want: 30 * time.Second},
		{name: "main model idle", working: false, want: 5 * time.Minute},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			now := time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)
			store := staticProgressStore{facts: &sessionstore.ProgressFacts{
				RootID: 7, EpisodeStartedAt: &now,
			}}
			runtime := New(
				store, nil,
				func(int64) bool { return true }, func(int64) bool { return tt.working }, nil, nil, nil,
			)

			assert.Equal(t, tt.want, runtime.Reconcile(t.Context(), now))
		})
	}
}
