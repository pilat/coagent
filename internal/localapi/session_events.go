package localapi

import (
	"context"
	"fmt"

	"connectrpc.com/connect/v2"

	"github.com/pilat/coagent/api/coagent/v1"
	"github.com/pilat/coagent/api/coagent/v1/coagentv1connect"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionevent"
)

func (outputService) StreamSessionEvents(ctx context.Context, _ *coagentv1.StreamSessionEventsRequest,
	stream coagentv1connect.OutputServiceStreamSessionEventsServerStream,
) error {
	author, err := clientFrom(ctx)
	if err != nil {
		return err
	}

	events := author.control.Subscribe()
	defer author.control.Unsubscribe(events)

	if err := stream.Send(
		&coagentv1.StreamSessionEventsResponse{Type: coagentv1.SessionEventType_SESSION_EVENT_TYPE_ACTIVITY},
	); err != nil {
		return fmt.Errorf("send session event: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			return connect.Errorf(connect.CodeUnavailable, "the local API is closing")
		case event, ok := <-events:
			if !ok {
				return connect.Errorf(connect.CodeUnavailable, "session events ended")
			}

			if err := stream.Send(sessionEvent(event)); err != nil {
				return fmt.Errorf("send session event: %w", err)
			}
		}
	}
}

func sessionEvent(event controllerapi.SessionNotification) *coagentv1.StreamSessionEventsResponse {
	kind := coagentv1.SessionEventType_SESSION_EVENT_TYPE_ACTIVITY
	if event.Notification.Type == sessionevent.NotifyHeartbeat {
		kind = coagentv1.SessionEventType_SESSION_EVENT_TYPE_HEARTBEAT
	}

	return &coagentv1.StreamSessionEventsResponse{SessionId: event.SessionID, Type: kind}
}
