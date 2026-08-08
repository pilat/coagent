package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/ctl"
)

// A daemon whose socket is bound while its managers start must not send bare
// `coagent` home — it is coming up, so the install path's wait is the right answer.
func TestEnsureDaemon_WaitsOutABootingDaemon(t *testing.T) {
	srv, socket := newBootingDaemon(t)

	go func() {
		time.Sleep(200 * time.Millisecond)
		srv.MarkReady()
	}()

	st, code := ensureDaemon(context.Background(), socket)

	require.Equal(t, exitOK, code)
	assert.True(t, st.ConfigPresent)
}

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
