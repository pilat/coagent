package session

import (
	"context"
	"time"

	"github.com/pilat/coagent/internal/sessionevent"
)

func (s *Session) startHeartbeat(ctx context.Context) func() {
	ticker := time.NewTicker(time.Second)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-ticker.C:
				s.emit(sessionevent.Notification{Type: sessionevent.NotifyHeartbeat})
			case <-done:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	return func() {
		close(done)
		ticker.Stop()
	}
}
