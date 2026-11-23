package localapi

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/api/coagent/v1"
	"github.com/pilat/coagent/api/coagent/v1/coagentv1connect"
	"github.com/pilat/coagent/internal/controllerapi"
)

func TestServer_RefusesMissingAndWrongToken(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	h := f.start(t)
	for _, header := range []string{"", "Bearer ", "Bearer unknown-token"} {
		ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
		client := h.client(t, header)
		sessions := coagentv1connect.NewSessionServiceClient(client)
		_, err := sessions.CreateSession(ctx, &coagentv1.CreateSessionRequest{WorkDir: t.TempDir()})
		require.Error(t, err)
		assert.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err), "create with %q", header)
		_, err = sessions.SendInput(ctx, &coagentv1.SendInputRequest{
			SessionId: 1, Text: "hello", IdempotencyKey: "unauthenticated",
		})
		require.Error(t, err)
		assert.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err), "send with %q", header)
		stream, err := coagentv1connect.NewOutputServiceClient(client).
			TailOutput(ctx, &coagentv1.TailOutputRequest{SessionId: 1})
		if err == nil {
			_, err = stream.Receive()
			_ = stream.Close()
		}
		require.Error(t, err)
		assert.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err), "tail with %q", header)
		err = client.CallUnary(ctx, connect.Spec{
			Procedure: "/coagent.v1.SessionService/Nope", StreamType: connect.StreamTypeUnary,
		}, &coagentv1.SendInputRequest{}, &coagentv1.SendInputResponse{})
		cancel()
		require.Error(t, err)
		assert.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err), "unknown procedure with %q", header)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	err := h.client(t, "Bearer "+f.tokens["alpha"]).CallUnary(ctx, connect.Spec{
		Procedure: "/coagent.v1.SessionService/Nope", StreamType: connect.StreamTypeUnary,
	}, &coagentv1.SendInputRequest{}, &coagentv1.SendInputResponse{})
	require.Error(t, err)
	assert.Equal(t, connect.CodeUnimplemented, connect.CodeOf(err))
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localapi/", nil)
	require.NoError(t, err)
	response, err := h.httpClient(t).Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, response.StatusCode)
	f.backend.mu.Lock()
	defer f.backend.mu.Unlock()
	assert.Zero(t, f.backend.creates)
	assert.Empty(t, f.backend.sends)
}

func TestServer_RefusesGeneralOptionsWithoutToken(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	h := f.start(t)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodOptions, "http://localapi/", nil)
	require.NoError(t, err)
	request.URL.Opaque = "*"
	response, err := h.httpClient(t).Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, response.StatusCode)
}

func TestServer_ClientCannotReachAnotherClientsRoot(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	h := f.start(t)
	id := f.root(t, h, "alpha")
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	client := h.client(t, "Bearer "+f.tokens["beta"])
	_, err := coagentv1connect.NewSessionServiceClient(client).SendInput(ctx, &coagentv1.SendInputRequest{
		SessionId: id, Text: "private", IdempotencyKey: "foreign",
	})
	require.Error(t, err)
	assert.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))
	stream, err := coagentv1connect.NewOutputServiceClient(client).
		TailOutput(ctx, &coagentv1.TailOutputRequest{SessionId: id})
	if err == nil {
		defer func() { _ = stream.Close() }()
		var event *coagentv1.TailOutputResponse
		event, err = stream.Receive()
		assert.Nil(t, event)
	}
	require.Error(t, err)
	assert.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))
	f.backend.mu.Lock()
	defer f.backend.mu.Unlock()
	assert.Empty(t, f.backend.sends)
}

func TestCreateSession_RootBelongsToTheCallingClient(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	h := f.start(t)
	id := f.root(t, h, "alpha")
	record, err := f.store.GetSession(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, "alpha", record.Attributes[controllerapi.SessionAttributeManagerID])
	assert.Zero(t, record.ParentID)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	client := coagentv1connect.NewSessionServiceClient(h.client(t, "Bearer "+f.tokens["alpha"]))
	_, err = client.CreateSession(ctx, &coagentv1.CreateSessionRequest{WorkDir: "relative"})
	require.Error(t, err)
	assert.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))
}

func TestSendInput_PassesTextAndKeyAndRejectsMalformedRequests(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	h := f.start(t)
	id := f.root(t, h, "alpha")
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	client := coagentv1connect.NewSessionServiceClient(h.client(t, "Bearer "+f.tokens["alpha"]))
	text := "  /config keep this spacing\n"
	_, err := client.SendInput(ctx, &coagentv1.SendInputRequest{SessionId: id, Text: text, IdempotencyKey: "k1"})
	require.NoError(t, err)
	for _, request := range []*coagentv1.SendInputRequest{
		{SessionId: id, Text: "hello"},
		{SessionId: id, Text: "hello", IdempotencyKey: strings.Repeat("k", 129)},
		{SessionId: id, Text: " \n\t", IdempotencyKey: "blank"},
		{SessionId: 0, Text: "hello", IdempotencyKey: "zero"},
		{SessionId: -1, Text: "hello", IdempotencyKey: "negative"},
	} {
		_, err := client.SendInput(ctx, request)
		require.Error(t, err)
		assert.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))
	}
	f.backend.mu.Lock()
	defer f.backend.mu.Unlock()
	assert.Equal(t, []apiSend{{sessionID: id, text: text, key: "k1"}}, f.backend.sends)
}

func TestTailOutput_RejectsMalformedRequests(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	h := f.start(t)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	client := coagentv1connect.NewOutputServiceClient(h.client(t, "Bearer "+f.tokens["alpha"]))
	for _, request := range []*coagentv1.TailOutputRequest{
		{SessionId: 0},
		{SessionId: -1},
		{SessionId: 1, AfterId: -1},
	} {
		stream, err := client.TailOutput(ctx, request)
		if err == nil {
			_, err = stream.Receive()
			_ = stream.Close()
		}
		require.Error(t, err)
		assert.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))
	}
}

func TestCreateSession_PassesTheWorktreeName(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	client := coagentv1connect.NewSessionServiceClient(inProcessClient(t, f, "alpha"))
	_, err := client.CreateSession(
		t.Context(),
		&coagentv1.CreateSessionRequest{WorkDir: t.TempDir(), WorktreeName: "api"},
	)
	require.NoError(t, err)
	f.backend.mu.Lock()
	defer f.backend.mu.Unlock()
	assert.Equal(t, []string{"api"}, f.backend.worktrees)
}
