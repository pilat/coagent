package telegram

import (
	"context"
	"time"

	"github.com/pilat/coagent/api/coagent/v1"
	"github.com/pilat/coagent/api/coagent/v1/coagentv1connect"
)

func (m *Manager) notificationsLoop(
	ctx context.Context,
	watch coagentv1connect.OutputServiceWatchSessionsClientStream,
) {
	for ctx.Err() == nil {
		event, err := watch.Receive()
		if err == nil {
			m.handleNotification(ctx, event)
			continue
		}

		_ = watch.Close()

		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(reconnectBackoffBase):
			}

			next, err := m.api.outputs.WatchSessions(ctx, &coagentv1.WatchSessionsRequest{})
			if err == nil {
				watch = next
				break
			}
		}
	}
}

func (m *Manager) handleNotification(ctx context.Context, event *coagentv1.WatchSessionsResponse) {
	m.wakeDelivery()

	if event.GetType() != coagentv1.SessionEventType_SESSION_EVENT_TYPE_HEARTBEAT {
		return
	}

	m.mu.RLock()
	delivery := m.delivery
	m.mu.RUnlock()

	if delivery != nil && event.GetSessionId() > 0 && delivery.Focus() == event.GetSessionId() {
		_ = m.sendTyping(ctx)
	}
}

func (m *Manager) wakeDelivery() {
	m.mu.RLock()
	delivery := m.delivery
	m.mu.RUnlock()

	if delivery != nil {
		delivery.Wake()
	}
}
