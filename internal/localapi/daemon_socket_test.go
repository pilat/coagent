package localapi

import (
	"context"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/api/coagent/v1"
	"github.com/pilat/coagent/internal/config"
)

func TestStatusSocket_ServesOnlyGetStatus(t *testing.T) {
	f := newAPIFixture(t)
	h := f.start(t)
	h.srv.MarkReady()
	client := (&apiServer{socket: h.statusSocket}).httpClient(t)
	services := coagentv1.File_coagent_v1_coagent_proto.Services()
	for i := range services.Len() {
		service := services.Get(i)
		methods := service.Methods()
		for j := range methods.Len() {
			procedure := "/" + string(service.FullName()) + "/" + string(methods.Get(j).Name())
			t.Run(procedure, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
				defer cancel()
				request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localapi"+procedure,
					strings.NewReader("{}"))
				require.NoError(t, err)
				request.Header.Set("Content-Type", "application/json")
				response, err := client.Do(request)
				require.NoError(t, err)
				require.NoError(t, response.Body.Close())
				want := http.StatusNotFound
				if procedure == "/coagent.v1.DaemonService/GetStatus" {
					want = http.StatusOK
				}
				assert.Equal(t, want, response.StatusCode)
			})
		}
	}
}

func TestStatus_AnswersStartingUntilReady(t *testing.T) {
	h := newStatusHarness(t, &config.Config{}, false)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	_, err := h.client.GetStatus(ctx, &coagentv1.GetStatusRequest{})
	var remote *connect.Error
	require.ErrorAs(t, err, &remote)
	assert.True(t, remote.IsRemote())
	assert.Equal(t, connect.CodeUnavailable, remote.Code())

	h.server.MarkReady()
	response, err := h.client.GetStatus(ctx, &coagentv1.GetStatusRequest{})
	require.NoError(t, err)
	assert.Equal(t, statusTestVersion, response.BinaryVersion)
}

func TestServer_CloseRemovesBothSockets(t *testing.T) {
	h := newAPIFixture(t).start(t)
	for _, socket := range []string{h.socket, h.statusSocket} {
		_, err := os.Stat(socket)
		require.NoError(t, err)
	}
	h.close(t)
	for _, socket := range []string{h.socket, h.statusSocket} {
		_, err := os.Stat(socket)
		require.ErrorIs(t, err, os.ErrNotExist)
	}
}

func TestListenUnix_ReplacesAStaleSocket(t *testing.T) {
	previous, path := apiListener(t)
	require.NoError(t, previous.Close())
	require.NoError(t, os.WriteFile(path, []byte("stale"), 0o600))
	listener, err := ListenUnix(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSocket)

	accepted := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			err = conn.Close()
		}
		accepted <- err
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", path)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	select {
	case err := <-accepted:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("replacement socket did not accept a connection")
	}
}

func TestListenUnix_SocketIsOwnerOnly(t *testing.T) {
	previous, path := apiListener(t)
	require.NoError(t, previous.Close())
	listener, err := ListenUnix(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestServer_AttachAPIRefusesAnUnbindableClient(t *testing.T) {
	h := newStatusHarness(t, &config.Config{}, true)
	listener, _ := apiListener(t)
	t.Cleanup(func() { _ = listener.Close() })
	err := h.server.AttachAPI(listener, []Client{{ID: "bad", TokenSHA256: "invalid"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token hash")
	assert.Equal(t, statusTestVersion, h.status(t).BinaryVersion)
}
