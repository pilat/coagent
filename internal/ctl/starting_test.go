package ctl

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
)

// A bound socket that has not answered yet is a daemon coming up: connect succeeds,
// so "not running" is wrong and a bare transport error aborts a healthy boot.
func TestDial_BoundButSilentIsStarting(t *testing.T) {
	shortDialTimeout(t)

	socket := socketPath(t)

	srv, err := NewServer(context.Background(), socket, testVersion, Deps{Config: &config.Config{}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })

	_, err = Dial(context.Background(), socket)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrNotRunning)
	require.ErrorIs(t, err, ErrStarting)
}

// Every op answers "starting" while the control plane is assembled, including one
// already registered: a half-built registry must never read as "unknown method".
func TestServer_StartingPhaseRefusesEveryOpWithOneAnswer(t *testing.T) {
	srv, socket := newStartingServer(t)

	require.NoError(t, srv.Register("chat_open", func(context.Context, *Conn, json.RawMessage) (any, *Error) {
		return "opened", nil
	}))

	c, err := Dial(context.Background(), socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	_, err = c.Status(context.Background())
	require.ErrorIs(t, err, ErrStarting)

	require.ErrorIs(t, c.Call(context.Background(), "chat_open", nil, nil), ErrStarting)
	require.ErrorIs(t, c.Call(context.Background(), "no_such_op", nil, nil), ErrStarting)

	srv.MarkReady()

	// The same connection carries on: a client that dialled during the boot does
	// not have to reconnect to see a ready daemon.
	st, err := c.Status(context.Background())
	require.NoError(t, err)
	assert.Equal(t, testVersion, st.BinaryVersion)

	var out string

	require.NoError(t, c.Call(context.Background(), "chat_open", nil, &out))
	assert.Equal(t, "opened", out)
}

// Registration stays open through the starting phase — that is what the phase is
// for — and closes when the daemon declares itself ready.
func TestServer_RegistrationClosesOnReadyNotOnAccept(t *testing.T) {
	srv, _ := newStartingServer(t)

	handler := func(context.Context, *Conn, json.RawMessage) (any, *Error) { return nil, nil }

	require.NoError(t, srv.Register("during_boot", handler))

	srv.MarkReady()

	require.ErrorIs(t, srv.Register("after_ready", handler), ErrRegistrationClosed)
}

func TestServer_UnknownMethodOnAReadyDaemonIsNotStarting(t *testing.T) {
	c := newHarness(t, &config.Config{}).dial(t)

	err := c.Call(context.Background(), "no_such_op", nil, nil)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrStarting)
}

// newStartingServer binds and accepts without declaring readiness — the daemon's
// state while its managers are starting.
func newStartingServer(t *testing.T) (*Server, string) {
	t.Helper()

	socket := socketPath(t)

	srv, err := NewServer(context.Background(), socket, testVersion, Deps{Config: &config.Config{}})
	require.NoError(t, err)

	go func() { _ = srv.ServeStarting(context.Background()) }()
	<-srv.serveReady

	t.Cleanup(func() { _ = srv.Close() })

	return srv, socket
}

// shortDialTimeout keeps the bound-but-silent probe from spending the full
// production budget.
func shortDialTimeout(t *testing.T) {
	t.Helper()

	original := dialTimeout
	dialTimeout = 150 * time.Millisecond

	t.Cleanup(func() { dialTimeout = original })
}
