package session

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHeartbeatStopJoinsCallbackAndSerializesRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered := make(chan context.Context, 4)
		cancelled := make(chan struct{}, 4)
		release := make(chan struct{})
		var releaseOnce sync.Once
		var calls atomic.Int32
		h := newHeartbeatTicker(func(ctx context.Context) {
			n := calls.Add(1)
			entered <- ctx
			<-ctx.Done()
			cancelled <- struct{}{}
			if n == 1 {
				<-release
			}
		})
		defer func() {
			releaseOnce.Do(func() { close(release) })
			h.stop()
		}()
		h.start(t.Context())
		h.start(t.Context())
		first := awaitHeartbeat(t, entered)
		stopped := make(chan struct{})
		go func() { h.stop(); close(stopped) }()
		awaitHeartbeat(t, cancelled)
		require.ErrorIs(t, first.Err(), context.Canceled)
		synctest.Wait()
		select {
		case <-stopped:
			t.Fatal("stop returned while its callback was still running")
		default:
		}

		h.start(t.Context())
		// Advance two ticker periods while the old callback cannot finish.
		time.Sleep(2 * time.Second)
		assert.Equal(t, int32(1), calls.Load(), "start during stop must not overlap generations")
		releaseOnce.Do(func() { close(release) })
		awaitHeartbeat(t, stopped)
		h.stop()

		h.start(t.Context())
		second := awaitHeartbeat(t, entered)
		assert.NotSame(t, first, second)
		stoppedAgain := make(chan struct{})
		go func() { h.stop(); close(stoppedAgain) }()
		awaitHeartbeat(t, cancelled)
		awaitHeartbeat(t, stoppedAgain)
		assert.Equal(t, int32(2), calls.Load())
		h.stop()
	})
}

func TestHeartbeatRecoversPanicAndJoinsAfterParentCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		entered := make(chan int32, 4)
		var calls atomic.Int32
		h := newHeartbeatTicker(func(context.Context) {
			n := calls.Add(1)
			entered <- n
			if n == 1 {
				panic("heartbeat callback failed")
			}
		})
		defer h.stop()
		h.start(ctx)
		assert.Equal(t, int32(1), awaitHeartbeat(t, entered))
		assert.Equal(t, int32(2), awaitHeartbeat(t, entered))
		cancel()
		h.stop()
		time.Sleep(2 * time.Second)
		assert.Equal(t, int32(2), calls.Load())
		h.stop()
	})
}

func TestHeartbeatNilCallbackAndStoppedWorkerAreNoOps(t *testing.T) {
	h := newHeartbeatTicker(nil)
	h.start(t.Context())
	h.start(t.Context())
	h.stop()
	h.stop()
}

func awaitHeartbeat[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("heartbeat lifecycle did not reach the expected boundary")
		var zero T
		return zero
	}
}
