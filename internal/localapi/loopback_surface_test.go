package localapi

import (
	"context"
	"fmt"
	"io"
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

func TestLoopback_KeepsTheAPISocketGuarantees(t *testing.T) {
	f := newAPIFixture(t)
	listener, _ := loopbackListener(t)
	h := f.serveLoopback(t, listener, []string{"http://localhost:5173"})
	client := fixtureHTTPClient(t, "tcp", h.addr)
	for _, tc := range []struct {
		name, method, path, origin string
		token                      bool
		status                     int
	}{
		{name: "list without token", path: "/coagent.v1.SessionService/ListSessions", status: 401},
		{name: "list with token", path: "/coagent.v1.SessionService/ListSessions", token: true, status: 200},
		{name: "status with token", path: "/coagent.v1.DaemonService/GetStatus", token: true, status: 404},
		{name: "status without token", path: "/coagent.v1.DaemonService/GetStatus", status: 401},
		{name: "unknown with token", path: "/nope.Service/Missing", token: true, status: 404},
		{name: "unknown without token", path: "/nope.Service/Missing", status: 401},
		{name: "general options", method: "OPTIONS", path: "*", status: 401},
		{name: "get root", method: "GET", path: "/", status: 401},
		{
			name: "browser auth refusal", path: "/coagent.v1.SessionService/ListSessions",
			origin: "http://localhost:5173", status: 401,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			method := tc.method
			if method == "" {
				method = http.MethodPost
			}
			req, err := http.NewRequestWithContext(t.Context(), method, "http://"+h.addr+"/", strings.NewReader("{}"))
			require.NoError(t, err)
			if tc.path == "*" {
				req.URL.Opaque = "*"
			} else {
				req.URL.Path = tc.path
			}
			req.Header.Set("Content-Type", "application/json")
			if tc.token {
				req.Header.Set("Authorization", "Bearer "+f.tokens["alpha"])
			}
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			response, err := client.Do(req)
			require.NoError(t, err)
			defer func() { _ = response.Body.Close() }()
			_, err = io.Copy(io.Discard, response.Body)
			require.NoError(t, err)
			assert.Equal(t, tc.status, response.StatusCode)
			assert.Equal(t, tc.origin, response.Header.Get("Access-Control-Allow-Origin"))
			assert.Empty(t, response.Header.Get("Access-Control-Allow-Credentials"))
		})
	}
}

func TestLoopback_RemovesTheBearerFromTheHTTPRequest(t *testing.T) {
	f := newAPIFixture(t)
	listener, addr := loopbackListener(t)
	h := f.serveLoopback(t, listener, nil)
	body := strings.NewReader(fmt.Sprintf(`{"work_dir":%q}`, t.TempDir()))
	request := httptest.NewRequest(http.MethodPost, "/coagent.v1.SessionService/CreateSession", body)
	request.Host = addr
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+f.tokens["alpha"])
	response := httptest.NewRecorder()
	h.srv.loopback.Handler.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code)
	assert.Empty(t, request.Header.Get("Authorization"))
}

func TestLoopback_AppKeepsNoChat(t *testing.T) {
	f := newAPIFixture(t)
	f.entries[0].Control = f.factory.ForApp("alpha", nil)
	listener, _ := loopbackListener(t)
	h := f.serveLoopback(t, listener, []string{"http://localhost:5173"})
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	chat := coagentv1connect.NewChatServiceClient(h.tcpClient(t, "Bearer "+f.tokens["alpha"], "http://localhost:5173"))
	_, err := chat.BindChat(ctx, &coagentv1.BindChatRequest{ChatId: "chat"})
	require.Error(t, err)
	assert.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))
	assert.Contains(t, err.Error(), "an app keeps no chat; follow a root with TailOutput")
}
