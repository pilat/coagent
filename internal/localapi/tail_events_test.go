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

func TestTailEvents_DrainsWithoutAReader(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	events := make(chan controllerapi.SessionNotification, 64)
	drain := &tailEvents{wake: make(chan struct{}, 1)}
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
		return len(events) == 0 && drain.stateSession == 42
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
