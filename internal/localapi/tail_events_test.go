package localapi

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionevent"
)

func TestTailEvents_PreservesTailedRootStateAfterAnotherRootChanges(t *testing.T) {
	t.Parallel()
	events := make(chan controllerapi.SessionNotification, 2)
	drain := &tailEvents{
		wake:   make(chan struct{}, 1),
		states: make(map[int64]sessionevent.State),
	}
	events <- controllerapi.SessionNotification{
		SessionID: 42,
		Notification: sessionevent.Notification{
			Type: sessionevent.NotifyStateChanged, Status: sessionevent.StateRunning,
		},
	}
	events <- controllerapi.SessionNotification{
		SessionID: 43,
		Notification: sessionevent.Notification{
			Type: sessionevent.NotifyStateChanged, Status: sessionevent.StateIdle,
		},
	}
	close(events)
	drain.run(t.Context(), events)
	state, ok := drain.takeState(42)
	require.True(t, ok, "another root's event must not erase the tailed root's pending state")
	assert.Equal(t, sessionevent.StateRunning, state)
	state, ok = drain.takeState(43)
	require.True(t, ok)
	assert.Equal(t, sessionevent.StateIdle, state)
	for _, id := range []int64{42, 43} {
		_, ok := drain.takeState(id)
		assert.False(t, ok)
	}
}

func TestTailEvents_DrainsWithoutAReader(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	events := make(chan controllerapi.SessionNotification, 64)
	drain := &tailEvents{
		wake:   make(chan struct{}, 1),
		states: make(map[int64]sessionevent.State),
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		drain.run(ctx, events)
	}()
	for i := range 100 {
		n := sessionevent.Notification{Type: sessionevent.NotifyHeartbeat}
		if i == 99 {
			n = sessionevent.Notification{Type: sessionevent.NotifyStateChanged, Status: sessionevent.StateRunning}
		}
		select {
		case events <- controllerapi.SessionNotification{SessionID: 42, Notification: n}:
		case <-ctx.Done():
			t.Fatal("subscription drain blocked its producer")
		}
	}
	require.Eventually(t, func() bool {
		drain.mu.Lock()
		defer drain.mu.Unlock()
		return len(events) == 0 && drain.states[42] == sessionevent.StateRunning
	}, time.Second, time.Millisecond)
	assert.Len(t, drain.wake, 1)
	state, ok := drain.takeState(42)
	assert.True(t, ok)
	assert.Equal(t, sessionevent.StateRunning, state)
	_, ok = drain.takeState(42)
	assert.False(t, ok)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("subscription drain did not stop")
	}
}
