//go:build integration

package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/api/coagent/v1"
	"github.com/pilat/coagent/api/coagent/v1/coagentv1connect"
	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/localapi"
)

func TestHarnessE2E_LocalAPICreatesSendsAndTailsAcrossRestart(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "coa-api")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	binary := buildBinary(t)
	token, apiBlock := localAPIE2EToken(t, binary, home)
	catalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/models" {
			http.NotFound(w, r)

			return
		}

		_, _ = fmt.Fprintf(w, `{"data":[`+catalogDropModel+`]}`, "kept", "Kept")
	}))
	t.Cleanup(catalog.Close)
	dir := filepath.Join(home, coagenthome.DirName)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	document := strings.Replace(
		fmt.Sprintf(catalogDropConfig, catalog.URL),
		"  - id: vendor/dropped\n    provider: fake\n",
		"",
		1,
	)
	require.NoError(t, os.WriteFile(filepath.Join(dir, coagenthome.ConfigFileName), []byte(document+apiBlock), 0o600))
	socket := filepath.Join(dir, coagenthome.APISocketFileName)
	controlSocket := filepath.Join(dir, coagenthome.SocketFileName)
	process := startDaemon(t, binary, home)
	waitForStatus(
		t,
		controlSocket,
		func(st *coagentv1.GetStatusResponse) bool {
			return st.Pid == int64(process.Process.Pid) && st.ModelCount == 1
		},
	)
	waitForLocalAPIE2E(t, socket)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	unauthorized := coagentv1connect.NewSessionServiceClient(localAPIE2EClient(t, socket, ""))
	_, err = unauthorized.CreateSession(ctx, &coagentv1.CreateSessionRequest{WorkDir: t.TempDir()})
	require.Error(t, err)
	assert.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))
	client := localAPIE2EClient(t, socket, token)
	sessions := coagentv1connect.NewSessionServiceClient(client)
	created, err := sessions.CreateSession(
		ctx,
		&coagentv1.CreateSessionRequest{WorkDir: t.TempDir(), Model: "vendor/kept"},
	)
	require.NoError(t, err)
	id := created.GetSessionId()
	stream, err := coagentv1connect.NewOutputServiceClient(client).
		TailOutput(ctx, &coagentv1.TailOutputRequest{SessionId: id})
	require.NoError(t, err)
	defer func() { _ = stream.Close() }()
	var cursor int64
	opened := nextLocalAPIE2EOutput(t, stream, &cursor)
	assert.Equal(t, coagentv1.OutputType_OUTPUT_TYPE_SESSION_OPENED, opened.GetType())
	assert.Equal(t, id, opened.GetSessionId())

	for _, key := range []string{"s1", "s1", "s2"} {
		sendLocalAPIE2EInput(t, sessions, id, "/status", key)
	}

	for range 2 {
		row := nextLocalAPIE2EOutput(t, stream, &cursor)
		assert.Equal(t, coagentv1.OutputType_OUTPUT_TYPE_MESSAGE, row.GetType())
		assert.Equal(t, id, row.GetSessionId())
	}

	stopDaemon(t, process)
	_ = stream.Close()
	cancel()
	lastBeforeRestart := cursor
	process = startDaemon(t, binary, home)
	defer stopDaemon(t, process)
	waitForStatus(
		t,
		controlSocket,
		func(st *coagentv1.GetStatusResponse) bool {
			return st.Pid == int64(process.Process.Pid) && st.ModelCount == 1
		},
	)
	waitForLocalAPIE2E(t, socket)
	client = localAPIE2EClient(t, socket, token)
	sessions = coagentv1connect.NewSessionServiceClient(client)

	for _, key := range []string{"s1", "s3"} {
		sendLocalAPIE2EInput(t, sessions, id, "/status", key)
	}

	resumedCtx, cancelResumed := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelResumed()
	resumed, err := coagentv1connect.NewOutputServiceClient(client).
		TailOutput(resumedCtx, &coagentv1.TailOutputRequest{SessionId: id, AfterId: cursor})
	require.NoError(t, err)
	defer func() { _ = resumed.Close() }()
	offline := nextLocalAPIE2EOutput(t, resumed, &cursor)
	assert.Equal(t, coagentv1.OutputType_OUTPUT_TYPE_MESSAGE, offline.GetType())
	assert.Equal(t, id, offline.GetSessionId())
	assert.Greater(t, offline.GetId(), lastBeforeRestart)
	sendLocalAPIE2EInput(t, sessions, id, "/status", "s4")
	live := nextLocalAPIE2EOutput(t, resumed, &cursor)
	assert.Equal(t, coagentv1.OutputType_OUTPUT_TYPE_MESSAGE, live.GetType())
	assert.Equal(t, id, live.GetSessionId())
	sendLocalAPIE2EInput(t, sessions, id, "/clear", "c1")
	replaced := nextLocalAPIE2EOutput(t, resumed, &cursor)
	require.Equal(t, coagentv1.OutputType_OUTPUT_TYPE_SESSION_REPLACED, replaced.GetType())
	require.NotEqual(t, id, replaced.GetSessionId())
	id = replaced.GetSessionId()
	cleared := nextLocalAPIE2EOutput(t, resumed, &cursor)
	assert.Equal(t, id, cleared.GetSessionId())
	assert.Equal(t, coagentv1.OutputType_OUTPUT_TYPE_MESSAGE, cleared.GetType())
	assert.Equal(t, "Session cleared.", cleared.GetContent())
	sendLocalAPIE2EInput(t, sessions, id, "/kill", "k1")
	ack := nextLocalAPIE2EOutput(t, resumed, &cursor)
	assert.Equal(t, id, ack.GetSessionId())
	assert.Equal(t, coagentv1.OutputType_OUTPUT_TYPE_MESSAGE, ack.GetType())
	assert.Equal(t, "Stopping session...", ack.GetContent())
	closed := nextLocalAPIE2EOutput(t, resumed, &cursor)
	assert.Equal(t, id, closed.GetSessionId())
	assert.Equal(t, coagentv1.OutputType_OUTPUT_TYPE_SESSION_CLOSED, closed.GetType())

	for {
		event, err := resumed.Receive()
		if err != nil {
			require.ErrorIs(t, err, io.EOF)

			break
		}

		require.Nil(t, event.GetOutput(), "unexpected output after the closing row")
	}
}

func localAPIE2EToken(t *testing.T, binary, home string) (string, string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	command := exec.CommandContext(ctx, binary, "token", "e2e")
	command.Env = isolatedProcessEnv(os.Environ(), home)
	printed, err := command.CombinedOutput()
	require.NoError(t, err, "%s", printed)
	lines := strings.Split(string(printed), "\n")
	require.GreaterOrEqual(t, len(lines), 2)
	token := lines[1]
	_, block, found := strings.Cut(string(printed), "\napi:\n")
	require.True(t, found)
	block = "api:\n" + block
	cfg, err := config.ParseAndResolve([]byte(block), nil)
	require.NoError(t, err)
	require.Len(t, cfg.API.Clients, 1)
	assert.Equal(t, "e2e", cfg.API.Clients[0].ID)
	assert.Equal(t, localapi.TokenHash(token), cfg.API.Clients[0].TokenSHA256)

	return token, block
}

func waitForLocalAPIE2E(t *testing.T, socket string) {
	t.Helper()

	require.Eventually(t, func() bool {
		if _, err := os.Stat(socket); err != nil {
			return false
		}

		conn, err := net.DialTimeout("unix", socket, 100*time.Millisecond)
		if err != nil {
			return false
		}

		_ = conn.Close()

		return true
	}, 10*time.Second, 20*time.Millisecond)
}

func localAPIE2EClient(t *testing.T, socket, token string) *connect.Client {
	t.Helper()

	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dialer net.Dialer

		return dialer.DialContext(ctx, "unix", socket)
	}}
	t.Cleanup(transport.CloseIdleConnections)

	return connect.NewClient(connecthttp.NewTransport(&http.Client{Transport: transport}, "http://localapi"),
		func(next connect.ClientFunc) connect.ClientFunc {
			return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
				info, ok := connect.CallInfoForClientContext(ctx)
				if ok && token != "" {
					info.RequestHeader().Set("Authorization", "Bearer "+token)
				}

				return next(ctx, spec)
			}
		})
}

func sendLocalAPIE2EInput(t *testing.T, client coagentv1connect.SessionServiceClient, id int64, text, key string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	_, err := client.SendInput(ctx, &coagentv1.SendInputRequest{SessionId: id, Text: text, IdempotencyKey: key})
	require.NoError(t, err)
}

func nextLocalAPIE2EOutput(
	t *testing.T,
	stream coagentv1connect.OutputServiceTailOutputClientStream,
	after *int64,
) *coagentv1.OutputRow {
	t.Helper()

	for {
		event, err := stream.Receive()
		require.NoError(t, err)
		if row := event.GetOutput(); row != nil {
			require.Greater(t, row.GetId(), *after)
			*after = row.GetId()

			return row
		}
	}
}
