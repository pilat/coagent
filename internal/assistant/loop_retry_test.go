package assistant

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestAssistantLoopProviderFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newLoopFixture(t)
		f.client.err = errors.New("provider unavailable")
		f.enqueue("hello")
		started := time.Now()
		require.NoError(t, Run(t.Context(), f.store, f.host, f.root))
		assert.Equal(t, 4, f.client.calls)
		assert.Equal(t, []time.Time{
			started, started.Add(2 * time.Second),
			started.Add(7 * time.Second), started.Add(22 * time.Second),
		}, f.client.calledAt)
		assert.Equal(t, f.client.calls, f.client.closes)
		require.Len(t, f.host.notified, 1)
		assert.Contains(t, f.host.notified[0], "⚠️ turn failed:")
		assert.Contains(t, f.host.notified[0], "provider unavailable")
		var reason string
		require.NoError(
			t,
			f.db.QueryRowContext(t.Context(), `SELECT resolution_reason FROM session_inbox WHERE session_id = ?`, f.root).
				Scan(&reason),
		)
		assert.Equal(t, "assistant_turn_failed", reason)
		record, err := f.sessions.GetSession(t.Context(), f.root)
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
				require.ErrorIs(t, Run(ctx, f.store, f.host, f.root), context.DeadlineExceeded)
				assert.Equal(t, i+1, f.client.calls)
				assert.Equal(t, timeout, time.Since(started))
				assert.Empty(t, f.host.notified)
				inputs, err := f.sessions.ListPending(t.Context(), f.root)
				require.NoError(t, err)
				assert.Len(t, inputs, 1)
				record, err := f.sessions.GetSession(t.Context(), f.root)
				require.NoError(t, err)
				assert.Equal(t, sessionstore.SessionStatusCompleted, record.Status)
			})
		})
	}
}

func TestAssistantLoopModelErrorAppliesNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newLoopFixture(t, act(use(toolSend, `{"session":2,"text":"go"}`),
			use(toolRemember, `{"text":"never kept"}`)))
		f.client.err, f.client.failFrom = errors.New("provider unavailable"), 2
		f.enqueue("go")
		require.NoError(t, Run(t.Context(), f.store, f.host, f.root))
		assert.Equal(t, 5, f.client.calls)
		assert.Empty(t, f.host.moves, "a failed turn applies none of its buffered effects")
		require.Len(t, f.host.notified, 1)
		assert.Contains(t, f.host.notified[0], "⚠️ turn failed:")
		memory, err := f.store.Memory(t.Context(), f.root)
		require.NoError(t, err)
		assert.Empty(t, memory)
		assert.Equal(t, []string{"failed"}, f.callOutcomes())
		var reason string
		require.NoError(
			t,
			f.db.QueryRowContext(t.Context(), `SELECT resolution_reason FROM session_inbox WHERE session_id = ?`, f.root).
				Scan(&reason),
		)
		assert.Equal(t, "assistant_turn_failed", reason)
	})
}

func TestAssistantLoopGuardFailureCommitsTheReply(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newLoopFixture(t, act(use(toolSend, `{"session":2,"text":"go"}`)), say("Отправил."))
		f.client.err, f.client.failFrom = errors.New("provider unavailable"), 3
		f.enqueue("go")
		require.NoError(t, Run(t.Context(), f.store, f.host, f.root))
		assert.Equal(t, 6, f.client.calls, "two main calls and one guard call with its retries")
		assert.Equal(t, []string{"Отправил."}, f.host.notified)
		assert.Equal(t, []string{"send 2 go"}, f.host.moves)
		assert.Equal(t, []string{"guard_unresolved"}, f.callOutcomes())
		var reason string
		require.NoError(
			t,
			f.db.QueryRowContext(t.Context(), `SELECT resolution_reason FROM session_inbox WHERE session_id = ?`, f.root).
				Scan(&reason),
		)
		assert.Equal(t, "assistant_turn", reason)
	})
}

func TestAssistantLoopRejectsCutOffReplies(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cut := &llmwire.Response{Text: "Половина отв", FinishType: llmwire.FinishLength}
		f := newLoopFixture(t, cut, cut, cut, cut)
		f.enqueue("hello")
		require.NoError(t, Run(t.Context(), f.store, f.host, f.root))
		require.Len(t, f.host.notified, 1)
		assert.Contains(t, f.host.notified[0], "⚠️ turn failed:")
		assert.Contains(t, f.host.notified[0], "stopped early: length")
	})
}
