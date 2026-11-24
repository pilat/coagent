package localapi

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/api/coagent/v1"
	"github.com/pilat/coagent/api/coagent/v1/coagentv1connect"
)

func TestSendInput_AcceptedContextCannotYieldTheBearerToken(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	created := make(chan context.Context, 1)
	sent := make(chan context.Context, 1)
	f.backend.create = func(ctx context.Context) { created <- ctx }
	f.backend.send = func(ctx context.Context) error {
		sent <- ctx
		return nil
	}
	h := f.start(t)
	id := f.root(t, h, "alpha")
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	client := coagentv1connect.NewSessionServiceClient(h.client(t, "Bearer "+f.tokens["alpha"]))
	_, err := client.SendInput(ctx, &coagentv1.SendInputRequest{
		SessionId: id, Text: "hello", IdempotencyKey: "credential-context",
	})
	require.NoError(t, err)
	for name, captured := range map[string]context.Context{"send": <-sent, "create": <-created} {
		t.Run(name, func(t *testing.T) {
			info, hasInfo := connect.CallInfoForServerContext(captured)
			require.True(t, hasInfo, "the inherited context carries sanitized RPC metadata")
			assert.False(t, info.RequestHeader().Has("Authorization"))
			if name == "send" {
				_, hasDeadline := captured.Deadline()
				assert.False(t, hasDeadline, "accepted send must outlive the client deadline")
			}
		})
	}
}

func TestServer_RemovesTheBearerFromTheHTTPRequest(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	listener, _ := apiListener(t)
	t.Cleanup(func() { _ = listener.Close() })
	statusListener, _ := apiListener(t)
	t.Cleanup(func() { _ = statusListener.Close() })
	server := New(t.Context(), statusListener, StatusSource{})
	require.NoError(t, server.AttachAPI(listener, f.clients()))
	t.Cleanup(func() { require.NoError(t, server.Close(t.Context())) })
	body := strings.NewReader(fmt.Sprintf(`{"work_dir":%q}`, t.TempDir()))
	request := httptest.NewRequest(http.MethodPost, "/coagent.v1.SessionService/CreateSession", body)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+f.tokens["alpha"])
	response := httptest.NewRecorder()
	server.api.Handler.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code)
	assert.Empty(t, request.Header.Get("Authorization"), "the real request must not retain the bearer")
}

func TestAuthenticate_DropsTheBearerBeforeCallingTheHandler(t *testing.T) {
	t.Parallel()
	token, err := NewToken()
	require.NoError(t, err)
	expected := &client{hash: sha256.Sum256([]byte(token))}
	called := false
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Authorization"))
		author, err := clientFrom(r.Context())
		require.NoError(t, err)
		assert.Same(t, expected, author)
		called = true
	})
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	authenticate([]*client{expected}, next).ServeHTTP(httptest.NewRecorder(), request)
	assert.True(t, called)
	_, hasAuthorization := request.Header["Authorization"]
	assert.False(t, hasAuthorization)
}

func TestAuthenticate_RefusesNonRPCWithoutReadingItsBody(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		header  string
		message string
	}{
		{header: "", message: "missing bearer token"},
		{header: "Bearer ", message: "missing bearer token"},
		{header: "Bearer unknown", message: "invalid bearer token"},
	} {
		body := strings.NewReader("unread payload")
		request := httptest.NewRequest(http.MethodPost, "/", body)
		request.Header.Set("Content-Type", "text/plain")
		request.Header.Set("Authorization", tc.header)
		response := httptest.NewRecorder()
		next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Error("unauthenticated request reached the handler")
		})
		authenticate(nil, next).ServeHTTP(response, request)
		assert.Equal(t, http.StatusUnauthorized, response.Code)
		assert.Equal(t, tc.message+"\n", response.Body.String())
		assert.Equal(t, len("unread payload"), body.Len())
	}
}

func TestSessionService_WithoutAnAuthorRefusesCalls(t *testing.T) {
	t.Parallel()
	service := sessionService{}
	_, err := service.CreateSession(t.Context(), &coagentv1.CreateSessionRequest{WorkDir: "/project"})
	require.Error(t, err)
	assert.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))
	_, err = service.SendInput(t.Context(), &coagentv1.SendInputRequest{
		SessionId: 1, Text: "hello", IdempotencyKey: "no-author",
	})
	require.Error(t, err)
	assert.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))
}
