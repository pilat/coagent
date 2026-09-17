package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/ctl"
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
		name string
		err  error
		want int
	}{
		{name: "no daemon", err: ctl.ErrNotRunning, want: exitNotRunning},
		{name: "still booting", err: ctl.ErrStarting, want: exitNotRunning},
		// Only the sentinel counts: a message that merely reads like one is a
		// transport failure with unlucky wording.
		{name: "an error that only mentions starting", err: errors.New(ctl.ErrStarting.Error()), want: exitError},
		{name: "anything else", err: errors.New("write request: broken pipe"), want: exitError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, reportStatusFailure(tt.err))
		})
	}
}

// newBootingDaemon is a control socket that is bound and accepting but has not
// declared itself ready — what the daemon looks like while its managers start.
func newBootingDaemon(t *testing.T) (*ctl.Server, string) {
	t.Helper()

	socket := socketPath(t)

	srv, err := ctl.NewServer(context.Background(), socket, "test", ctl.Deps{
		Config: &config.Config{UnifiedConfig: &config.UnifiedConfig{}},
	})
	require.NoError(t, err)

	go func() { _ = srv.ServeStarting(context.Background()) }()

	t.Cleanup(func() { _ = srv.Close() })

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

// waitServing blocks until the socket answers with a greeting: the listener is
// bound before Serve runs, so connecting alone proves nothing.
func waitServing(t *testing.T, socket string) {
	t.Helper()

	require.Eventually(t, func() bool {
		client, err := ctl.Dial(context.Background(), socket)
		if err != nil {
			return false
		}

		_ = client.Close()

		return true
	}, 10*time.Second, 10*time.Millisecond)
}
