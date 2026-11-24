package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/localapi"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/managercontrol"
)

func TestStartLocalAPI_LoopbackListenerIsOptIn(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "off"
		if enabled {
			name = "on"
		}
		t.Run(name, func(t *testing.T) {
			localAPIHome(t)
			probe, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
			require.NoError(t, err)
			address := probe.Addr().String()
			require.NoError(t, probe.Close())
			cfg, token := loopbackAPIConfig(t)
			if enabled {
				cfg.UnifiedConfig.API.Listen = address
				cfg.UnifiedConfig.API.Origins = []string{"http://localhost:5173"}
			}
			core, logs := observer.New(zapcore.DebugLevel)
			a := &app{}
			stop := localAPIStops(t, a)
			_, err = startLocalAPI(logger.ToContext(t.Context(), zap.New(core)), a, cfg,
				managercontrol.New(nil, nil, nil, nil, nil), nil)
			require.NoError(t, err)
			assert.Equal(t, []string{"localapi"}, stopNames(a))
			if enabled {
				client := &http.Client{Timeout: 4 * time.Second}
				assert.Equal(t, 404, localAPIHTTPStatus(t, client, address, "", token))
				assert.Equal(t, 401, localAPIHTTPStatus(t, client, address, "", ""))
				_, port, err := net.SplitHostPort(address)
				require.NoError(t, err)
				assert.Equal(t, 403, localAPIHTTPStatus(t, client, address, "evil.example:"+port, token))
			} else {
				assertLoopbackClosed(t, address)
			}
			for _, entry := range logs.All() {
				assert.False(t, strings.HasPrefix(entry.Message, "local_api_"), entry.Message)
			}
			stop()
			assertLoopbackClosed(t, address)
		})
	}
}

func TestStartLocalAPI_LoopbackPortInUseLeavesTheAPISocketServing(t *testing.T) {
	home := localAPIHome(t)
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	cfg, token := loopbackAPIConfig(t)
	cfg.UnifiedConfig.API.Listen = listener.Addr().String()
	core, logs := observer.New(zapcore.DebugLevel)
	a := &app{}
	_ = localAPIStops(t, a)
	srv, err := startLocalAPI(logger.ToContext(t.Context(), zap.New(core)), a, cfg,
		managercontrol.New(nil, nil, nil, nil, nil), nil)
	require.NoError(t, err)
	entries := logs.FilterMessage("local_api_start_failed").All()
	require.Len(t, entries, 1)
	assert.Equal(t, cfg.UnifiedConfig.API.Listen, entries[0].ContextMap()["socket"])
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(
			ctx,
			"unix",
			filepath.Join(home, coagenthome.DirName, coagenthome.APISocketFileName),
		)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 4 * time.Second}
	assert.Equal(t, 404, localAPIHTTPStatus(t, client, "localhost", "", token))
	assert.Equal(t, 401, localAPIHTTPStatus(t, client, "localhost", "", ""))
	srv.MarkReady()
	_, connected, err := fetchStatus(t.Context(), filepath.Join(home, coagenthome.DirName, coagenthome.SocketFileName))
	require.NoError(t, err)
	assert.True(t, connected)
}

func localAPIHome(t *testing.T) string {
	t.Helper()
	home, err := os.MkdirTemp("/tmp", "api-loopback")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(home)) })
	t.Setenv("HOME", home)
	return home
}

func loopbackAPIConfig(t *testing.T) (*config.Config, string) {
	t.Helper()
	token, err := localapi.NewToken()
	require.NoError(t, err)
	return &config.Config{UnifiedConfig: &config.UnifiedConfig{API: config.APIConfig{
		Clients: []config.APIClientEntry{
			{ID: "test", Role: config.APIRoleOperator, TokenSHA256: localapi.TokenHash(token)},
		},
	}}}, token
}

func localAPIStops(t *testing.T, a *app) func() {
	t.Helper()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		for _, hook := range a.stops {
			require.NoError(t, hook.fn(context.Background()))
		}
	}
	t.Cleanup(stop)
	return stop
}

func localAPIHTTPStatus(t *testing.T, client *http.Client, address, host, token string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		"http://"+address+"/nope.Service/Missing", strings.NewReader("{}"))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if host != "" {
		req.Host = host
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	_, err = io.Copy(io.Discard, response.Body)
	require.NoError(t, err)
	return response.StatusCode
}

func assertLoopbackClosed(t *testing.T, address string) {
	t.Helper()
	conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(t.Context(), "tcp", address)
	if conn != nil {
		_ = conn.Close()
	}
	require.Error(t, err)
}
