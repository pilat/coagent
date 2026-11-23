package localapi

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/api/coagent/v1"
	"github.com/pilat/coagent/api/coagent/v1/coagentv1connect"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionevent"
)

var _ controllerapi.Controller = (*closedSubscriptionStub)(nil)

type closedSubscriptionStub struct {
	discoveryStub
}

func (s *closedSubscriptionStub) Subscribe() <-chan controllerapi.SessionNotification {
	events := make(chan controllerapi.SessionNotification)
	close(events)
	return events
}

func (s *closedSubscriptionStub) Unsubscribe(<-chan controllerapi.SessionNotification) {}

func TestStreamSessionEvents_FirstEventMarksTheSubscriptionLive(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	id := inProcessRoot(t, f, "alpha")
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	outputs := coagentv1connect.NewOutputServiceClient(inProcessClient(t, f, "alpha"))
	stream, err := outputs.StreamSessionEvents(ctx, &coagentv1.StreamSessionEventsRequest{})
	require.NoError(t, err)
	defer func() { _ = stream.Close() }()
	sync, err := stream.Receive()
	require.NoError(t, err)
	assert.Zero(t, sync.GetSessionId())
	assert.Equal(t, coagentv1.SessionEventType_SESSION_EVENT_TYPE_ACTIVITY, sync.GetType())
	f.wake(id)
	next, err := stream.Receive()
	require.NoError(t, err)
	assert.Equal(t, id, next.GetSessionId())
	assert.Equal(t, coagentv1.SessionEventType_SESSION_EVENT_TYPE_HEARTBEAT, next.GetType())
}

func TestStreamSessionEvents_CarriesOnlyOwnedHeartbeatsAndActivity(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	owned := inProcessRoot(t, f, "alpha")
	foreign := inProcessRoot(t, f, "beta")
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	outputs := coagentv1connect.NewOutputServiceClient(inProcessClient(t, f, "alpha"))
	stream, err := outputs.StreamSessionEvents(ctx, &coagentv1.StreamSessionEventsRequest{})
	require.NoError(t, err)
	defer func() { _ = stream.Close() }()
	_, err = stream.Receive()
	require.NoError(t, err)
	f.bus.Publish(owned, "alpha", sessionevent.Notification{Type: sessionevent.NotifyHeartbeat})
	f.bus.Publish(foreign, "beta", sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "private"})
	f.bus.Publish(owned, "alpha", sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: "owned"})
	for _, kind := range []coagentv1.SessionEventType{
		coagentv1.SessionEventType_SESSION_EVENT_TYPE_HEARTBEAT,
		coagentv1.SessionEventType_SESSION_EVENT_TYPE_ACTIVITY,
	} {
		event, err := stream.Receive()
		require.NoError(t, err)
		assert.Equal(t, owned, event.GetSessionId())
		assert.Equal(t, kind, event.GetType())
	}
}

func TestStreamSessionEvents_EndsWhenTheSubscriptionCloses(t *testing.T) {
	t.Parallel()
	transport, err := NewInProcess(t.Context(), apiStubFactory{control: &closedSubscriptionStub{}}).Transport("alpha")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	outputs := coagentv1connect.NewOutputServiceClient(connect.NewClient(transport))
	stream, err := outputs.StreamSessionEvents(ctx, &coagentv1.StreamSessionEventsRequest{})
	require.NoError(t, err)
	defer func() { _ = stream.Close() }()
	sync, err := stream.Receive()
	require.NoError(t, err)
	assert.Zero(t, sync.GetSessionId())
	assert.Equal(t, coagentv1.SessionEventType_SESSION_EVENT_TYPE_ACTIVITY, sync.GetType())
	_, err = stream.Receive()
	require.Error(t, err)
	assert.Equal(t, connect.CodeUnavailable, connect.CodeOf(err))
	refused, ok := errors.AsType[*connect.Error](err)
	require.True(t, ok)
	assert.Equal(t, "session events ended", refused.Message())
}
