package backgroundprocess

import (
	"context"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHarnessModel_ConcurrentPromotionAndCancellationReleaseOneClass(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	service := newTestService(t, store, nil, nil)
	spawn := func(ctx context.Context) (*exec.Cmd, error) {
		return exec.CommandContext(ctx, "sleep", "30"), nil
	}
	spec := testSpec(2)
	spec.Advertise = false

	for range 8 {
		process, err := service.Start(ctx, spec, spawn)
		require.NoError(t, err)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = service.Advertise(ctx, process.ID)
		}()
		go func() {
			defer wg.Done()
			_, _ = service.CancelProcess(ctx, process.ID, IntentAgentCancelled)
		}()
		wg.Wait()
		assert.Eventually(t, func() bool { return service.liveCount(2) == 0 }, 10*time.Second, 20*time.Millisecond)

		service.mu.Lock()
		assert.Zero(t, service.background[2])
		assert.Zero(t, service.candidates[2])
		assert.NotContains(t, service.classes, process.ID)
		service.mu.Unlock()
	}
}
