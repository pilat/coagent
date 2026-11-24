package localapi

import (
	"context"
	"sync"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionevent"
)

type tailEvents struct {
	wake   chan struct{}
	mu     sync.Mutex
	states map[int64]sessionevent.State
}

func (d *tailEvents) run(ctx context.Context, events <-chan controllerapi.SessionNotification) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-events:
			if !ok {
				return
			}

			if event.Notification.Type == sessionevent.NotifyStateChanged {
				d.mu.Lock()
				d.states[event.SessionID] = event.Notification.Status
				d.mu.Unlock()
			}

			select {
			case d.wake <- struct{}{}:
			default:
			}
		}
	}
}

func (d *tailEvents) takeState(sessionID int64) (sessionevent.State, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	state, ok := d.states[sessionID]
	delete(d.states, sessionID)

	return state, ok
}
