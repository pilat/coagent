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
}

func (panickingProgressStore) ListAutonomousProgressRoots(context.Context) ([]int64, error) {
	panic("store exploded")
}

// A panicking tick must not kill the reconciler goroutine: silence snapshots
// and duration-fire observation keep serving later deadlines.
func TestReconcileProgressSafelySurvivesStorePanic(t *testing.T) {
	t.Parallel()

	runtime := New(panickingProgressStore{}, nil, func(int64) bool { return false }, nil, nil)
	// The recovered panic is logged; a quiet logger keeps the test output honest.
	ctx := logger.ToContext(context.Background(), zap.NewNop())
	delay := runtime.Reconcile(ctx, time.Now().UTC())

	assert.Equal(t, SilenceInterval, delay,
		"the recovered tick must reschedule at the ordinary silence interval")
}
