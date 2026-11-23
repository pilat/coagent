package localapi

import (
	"context"
	"sync"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionevent"
)

type tailEvents struct {
	wake         chan struct{}
	mu           sync.Mutex
	stateSession int64
	state        sessionevent.State
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
				d.stateSession = event.SessionID
				d.state = event.Notification.Status
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

	if d.stateSession != sessionID {
		return "", false
	}

	state := d.state
	d.stateSession = 0

	return state, true
}
