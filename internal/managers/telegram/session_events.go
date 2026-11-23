package telegram

import (
	"context"
	"time"

	"github.com/pilat/coagent/api/coagent/v1"
	"github.com/pilat/coagent/api/coagent/v1/coagentv1connect"
)

func (m *Manager) notificationsLoop(
	ctx context.Context,
	events coagentv1connect.OutputServiceStreamSessionEventsClientStream,
) {
	for ctx.Err() == nil {
		event, err := events.Receive()
		if err == nil {
			m.handleNotification(ctx, event)
			continue
		}

		_ = events.Close()

		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(reconnectBackoffBase):
			}

			next, err := m.api.outputs.StreamSessionEvents(ctx, &coagentv1.StreamSessionEventsRequest{})
			if err == nil {
				events = next
				break
			}
		}
	}
}

func (m *Manager) handleNotification(ctx context.Context, event *coagentv1.StreamSessionEventsResponse) {
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
