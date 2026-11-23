package localapi

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/api/coagent/v1"
	"github.com/pilat/coagent/api/coagent/v1/coagentv1connect"
)

func TestServer_ServesTheNewServicesBehindTheToken(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	h := f.start(t)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	unauthenticated := h.client(t, "")
	_, err := coagentv1connect.NewChatServiceClient(unauthenticated).
		GetChatFocus(ctx, &coagentv1.GetChatFocusRequest{ChatId: "chat"})
	require.Error(t, err)
	assert.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))
	_, err = coagentv1connect.NewDiscoveryServiceClient(unauthenticated).ListModels(ctx, &coagentv1.ListModelsRequest{})
	require.Error(t, err)
	assert.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))
	stream, err := coagentv1connect.NewOutputServiceClient(unauthenticated).
		StreamSessionEvents(ctx, &coagentv1.StreamSessionEventsRequest{})
	if err == nil {
		_, err = stream.Receive()
		_ = stream.Close()
	}
	require.Error(t, err)
	assert.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))
	authenticated := h.client(t, "Bearer "+f.tokens["alpha"])
	focus, err := coagentv1connect.NewChatServiceClient(authenticated).
		GetChatFocus(ctx, &coagentv1.GetChatFocusRequest{ChatId: "chat"})
	require.NoError(t, err)
	assert.Zero(t, focus.GetSessionId())
	stream, err = coagentv1connect.NewOutputServiceClient(authenticated).
		StreamSessionEvents(ctx, &coagentv1.StreamSessionEventsRequest{})
	require.NoError(t, err)
	defer func() { _ = stream.Close() }()
	sync, err := stream.Receive()
	require.NoError(t, err)
	assert.Zero(t, sync.GetSessionId())
	assert.Equal(t, coagentv1.SessionEventType_SESSION_EVENT_TYPE_ACTIVITY, sync.GetType())
}
