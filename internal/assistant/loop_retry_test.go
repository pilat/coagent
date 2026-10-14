package assistant

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sessionstore"
)

func TestAssistantLoopProviderFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newLoopFixture(t)
		f.client.err = errors.New("provider unavailable")
		f.enqueue("hello")
		started := time.Now()
		require.NoError(t, Run(t.Context(), f.deps, f.root))
		assert.Equal(t, 4, f.client.calls)
		assert.Equal(t, []time.Time{
			started, started.Add(2 * time.Second),
			started.Add(7 * time.Second), started.Add(22 * time.Second),
		}, f.client.calledAt)
		assert.Equal(t, f.client.calls, f.client.closes)
		require.Len(t, f.notified, 1)
		assert.Contains(t, f.notified[0], "⚠️ turn failed:")
		assert.Contains(t, f.notified[0], "provider unavailable")
		var reason string
		require.NoError(
			t,
			f.db.QueryRowContext(t.Context(), `SELECT resolution_reason FROM session_inbox`).Scan(&reason),
		)
		assert.Equal(t, "assistant_turn_failed", reason)
		record, err := f.deps.Store.GetSession(t.Context(), f.root)
		require.NoError(t, err)
		assert.Equal(t, sessionstore.SessionStatusCompleted, record.Status)
	})
}

func TestAssistantLoopRetryCancellation(t *testing.T) {
	for i, timeout := range []time.Duration{time.Second, 3 * time.Second, 8 * time.Second} {
		t.Run(timeout.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newLoopFixture(t)
				f.client.err = errors.New("provider unavailable")
				f.enqueue("hello")
				started := time.Now()
				ctx, cancel := context.WithTimeout(t.Context(), timeout)
				defer cancel()
				require.ErrorIs(t, Run(ctx, f.deps, f.root), context.DeadlineExceeded)
				assert.Equal(t, i+1, f.client.calls)
				assert.Equal(t, timeout, time.Since(started))
				assert.Empty(t, f.notified)
				inputs, err := f.deps.Store.ListPending(t.Context(), f.root)
				require.NoError(t, err)
				assert.Len(t, inputs, 1)
				record, err := f.deps.Store.GetSession(t.Context(), f.root)
				require.NoError(t, err)
				assert.Equal(t, sessionstore.SessionStatusCompleted, record.Status)
			})
		})
	}
}
