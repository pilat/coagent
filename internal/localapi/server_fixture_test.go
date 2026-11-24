package localapi

import (
	"context"
	"net"
	"net/http"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"github.com/stretchr/testify/require"
)

func (f *apiFixture) serve(t *testing.T, listener net.Listener, socket string) *apiServer {
	t.Helper()
	return f.serveListeners(t, listener, socket, nil, nil)
}

func (f *apiFixture) serveLoopback(t *testing.T, listener net.Listener, origins []string) *apiServer {
	t.Helper()
	api, socket := apiListener(t)
	return f.serveListeners(t, api, socket, listener, origins)
}

func (f *apiFixture) serveListeners(
	t *testing.T,
	listener net.Listener,
	socket string,
	loopback net.Listener,
	origins []string,
) *apiServer {
	t.Helper()
	statusListener, statusSocket := apiListener(t)
	srv := New(t.Context(), statusListener, StatusSource{})
	require.NoError(t, srv.AttachAPI(listener, f.clients()))
	h := &apiServer{srv: srv, socket: socket, statusSocket: statusSocket, serving: 2}
	if loopback != nil {
		srv.AttachLoopback(loopback, origins)
		h.serving++
		h.addr = loopback.Addr().String()
	}
	h.done = make(chan error, h.serving)
	go func() { h.done <- srv.ServeStatus() }()
	go func() { h.done <- srv.ServeAPI() }()
	if loopback != nil {
		go func() { h.done <- srv.ServeLoopback() }()
	}
	t.Cleanup(func() { h.close(t) })
	return h
}

func (h *apiServer) httpClient(t *testing.T) *http.Client {
	t.Helper()
	return fixtureHTTPClient(t, "unix", h.socket)
}

func (h *apiServer) client(t *testing.T, authorization string) *connect.Client {
	t.Helper()
	return fixtureClient(h.httpClient(t), "http://localapi", authorization, "")
}

func (h *apiServer) tcpClient(t *testing.T, authorization, origin string) *connect.Client {
	t.Helper()
	return fixtureClient(fixtureHTTPClient(t, "tcp", h.addr), "http://"+h.addr, authorization, origin)
}

func fixtureHTTPClient(t *testing.T, network, address string) *http.Client {
	t.Helper()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport}
}

func fixtureClient(httpClient *http.Client, baseURL, authorization, origin string) *connect.Client {
	return connect.NewClient(connecthttp.NewTransport(httpClient, baseURL),
		func(next connect.ClientFunc) connect.ClientFunc {
			return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
				info, ok := connect.CallInfoForClientContext(ctx)
				if ok && authorization != "" {
					info.RequestHeader().Set("Authorization", authorization)
				}
				if ok && origin != "" {
					info.RequestHeader().Set("Origin", origin)
				}
				return next(ctx, spec)
			}
		})
}
