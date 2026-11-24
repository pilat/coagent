package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/localapi"
)

// maxSocketPath is the sun_path limit, checked here because a deep TMPDIR makes
// t.TempDir() unusable for a unix socket.
const maxSocketPath = 100

// `coagent status` must report a booting daemon as a retryable state, not as
// "could not ask" — a supervisor treats those differently.
func TestStatusOf_BootingDaemonIsRetryableNotAFailureToAsk(t *testing.T) {
	srv, socket := newBootingDaemon(t)

	assert.Equal(t, exitNotRunning, statusOf(context.Background(), socket))

	srv.MarkReady()

	assert.Equal(t, exitOK, statusOf(context.Background(), socket))
}

func TestReportStatusFailure(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		connected bool
		want      int
		stdout    string
	}{
		{
			name: "no daemon", err: errors.New("connection refused"),
			want: exitNotRunning, stdout: "daemon not running\n",
		},
		{
			name: "still booting", err: connect.NewError(connect.CodeUnavailable, "daemon is starting").WithRemote(),
			connected: true, want: exitNotRunning, stdout: "daemon starting — not answering yet\n",
		},
		{
			name: "bound but silent", err: context.DeadlineExceeded,
			connected: true, want: exitNotRunning, stdout: "daemon starting — not answering yet\n",
		},
		{
			name: "local unavailable", err: connect.NewError(connect.CodeUnavailable, "daemon is starting"),
			connected: true, want: exitError,
		},
		{
			name: "remote internal", err: connect.NewError(connect.CodeInternal, "broken").WithRemote(),
			connected: true, want: exitError,
		},
		{
			name: "an error that only mentions starting", err: errors.New("daemon is starting"),
			connected: true, want: exitError,
		},
		{
			name: "anything else", err: errors.New("write request: broken pipe"),
			connected: true, want: exitError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var code int
			var stderr string
			stdout := captureStatusStdout(t, func() {
				stderr = captureStatusStderr(t, func() { code = reportStatusFailure(tt.err, tt.connected) })
			})
			assert.Equal(t, tt.want, code)
			assert.Equal(t, tt.stdout, stdout)
			if tt.want == exitError {
				assert.Contains(t, stderr, tt.err.Error())
				assert.True(
					t,
					strings.HasSuffix(
						stderr,
						"the daemon may be running an older coagent binary — `sudo coagent restart` to apply this one\n",
					),
				)
			} else {
				assert.Empty(t, stderr)
			}
		})
	}
}

// newBootingDaemon is a control socket that is bound and accepting but has not
// declared itself ready — what the daemon looks like while its managers start.
func newBootingDaemon(t *testing.T) (*localapi.Server, string) {
	t.Helper()

	socket := socketPath(t)

	listener, err := localapi.ListenUnix(t.Context(), socket)
	require.NoError(t, err)
	srv := localapi.New(t.Context(), listener, localapi.StatusSource{
		Config: &config.Config{UnifiedConfig: &config.UnifiedConfig{}}, Version: "test",
	})

	done := make(chan error, 1)
	go func() { done <- srv.ServeStatus() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, srv.Close(ctx))
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-ctx.Done():
			t.Fatal("status server did not finish")
		}
	})

	waitServing(t, socket)

	return srv, socket
}

// socketPath keeps the unix path under the sun_path limit even when TMPDIR is
// deep, which a plain t.TempDir() join does not guarantee.
func socketPath(t *testing.T) string {
	t.Helper()

	if p := filepath.Join(t.TempDir(), "d.sock"); len(p) <= maxSocketPath {
		return p
	}

	short, err := os.MkdirTemp("/tmp", "coagentctl")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(short) })

	return filepath.Join(short, "d.sock")
}

// A connection alone proves no answer; require a remote status verdict.
func waitServing(t *testing.T, socket string) {
	t.Helper()

	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
		defer cancel()
		_, _, err := fetchStatus(ctx, socket)
		var remote *connect.Error

		return err == nil || errors.As(err, &remote) && remote.IsRemote()
	}, 10*time.Second, 10*time.Millisecond)
}
