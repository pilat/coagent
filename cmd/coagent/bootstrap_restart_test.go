package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/ctl"
)

// The draining image keeps answering on the same socket, so taking that for "the
// daemon is back" hands the chat a socket that is about to be unlinked.
func TestAddFirstProvider_DrainingImageDoesNotEndTheWait(t *testing.T) {
	shortDaemonWait(t, time.Second)

	socket := socketPath(t)
	first := startImage(t, socket, nil)

	run := driveAddFirstProvider(t, socket, first.applied, func() {})

	assert.Equal(t, exitNotRunning, run.code, "no new image ever answered")
	assert.Contains(t, run.out, "did not come up")
}

// A config the daemon cannot boot on is rolled back behind the bootstrap's back;
// "Saved." must not be the last word on a provider that is no longer there.
func TestAddFirstProvider_ReportsAProviderThatWasRolledBack(t *testing.T) {
	shortDaemonWait(t, 10*time.Second)

	socket := socketPath(t)
	first := startImage(t, socket, nil)

	run := driveAddFirstProvider(t, socket, first.applied, func() {
		restartImage(t, first, socket, nil)
	})

	assert.Equal(t, exitError, run.code)
	assert.Contains(t, run.out, "rolled back")
	assert.NotContains(t, run.out, "did not come up")
}

// The happy path: the new image answers with the provider, and only then does
// the bootstrap hand the terminal to the chat.
func TestAddFirstProvider_ReturnsOnceTheNewImageHasTheProvider(t *testing.T) {
	shortDaemonWait(t, 10*time.Second)

	socket := socketPath(t)
	first := startImage(t, socket, nil)

	run := driveAddFirstProvider(t, socket, first.applied, func() {
		restartImage(t, first, socket, []string{"anthropic"})
	})

	assert.Equal(t, exitOK, run.code)
	assert.Contains(t, run.out, "Saved.")
	assert.NotContains(t, run.out, "rolled back")
}

// The wait is over a run of the daemon, not over "something answered": the run
// that took the change is the one thing that must not end it.
func TestWaitForReboot_RejectsTheRunItWasGiven(t *testing.T) {
	shortDaemonWait(t, time.Second)

	socket := socketPath(t)
	startImage(t, socket, nil)

	_, code := waitForReboot(context.Background(), socket, bootIDOf(t, socket))

	assert.Equal(t, exitNotRunning, code)
}

// A daemon too old to report a boot id leaves nothing to compare — waiting for a
// signal it will never send is worse than the plain wait.
func TestWaitForReboot_WithoutAPreviousRunTakesAnyDaemon(t *testing.T) {
	shortDaemonWait(t, 5*time.Second)

	socket := socketPath(t)
	startImage(t, socket, nil)

	st, code := waitForReboot(context.Background(), socket, "")

	require.Equal(t, exitOK, code)
	assert.NotEmpty(t, st.BootID)
}

type bootstrapRun struct {
	code int
	out  string
}

// driveAddFirstProvider runs the provider step and fires onApplied once the daemon
// has answered the write — where the real one begins its drain.
func driveAddFirstProvider(t *testing.T, socket string, applied <-chan struct{}, onApplied func()) bootstrapRun {
	t.Helper()

	answerStdin(t, "1\n")

	codes := make(chan int, 1)

	var run bootstrapRun

	run.out = captureOutput(t, func() {
		go func() { codes <- addFirstProvider(context.Background(), socket) }()

		select {
		case <-applied:
		case <-time.After(30 * time.Second):
			t.Error("the bootstrap never asked for a provider")

			return
		}

		onApplied()

		select {
		case run.code = <-codes:
		case <-time.After(60 * time.Second):
			t.Error("addFirstProvider never returned")
		}
	})

	return run
}

// daemonImage is one control server standing in for one daemon image; applied
// closes when a set_provider answer is on the wire, where the real drain starts.
type daemonImage struct {
	srv     *ctl.Server
	applied chan struct{}
}

// startImage brings up an image on the bootstrap's socket: it answers status
// from the providers it was given and accepts set_provider.
func startImage(t *testing.T, socket string, providers []string) *daemonImage {
	t.Helper()

	entries := make(map[string]config.ProviderEntry, len(providers))
	for _, name := range providers {
		entries[name] = config.ProviderEntry{Driver: "anthropic"}
	}

	srv, err := ctl.NewServer(context.Background(), socket, "test", ctl.Deps{
		Config: &config.Config{UnifiedConfig: &config.UnifiedConfig{Providers: entries}},
	})
	require.NoError(t, err)

	applied := make(chan struct{})

	handler := func(_ context.Context, c *ctl.Conn, _ json.RawMessage) (any, *ctl.Error) {
		c.AfterReply(func() { close(applied) })

		return configops.OK(), nil
	}
	require.NoError(t, srv.Register(ctl.OpSetProvider, handler))

	go func() { _ = srv.Serve(context.Background()) }()

	t.Cleanup(func() { _ = srv.Close() })

	waitServing(t, socket)

	return &daemonImage{srv: srv, applied: applied}
}

// restartImage is what the daemon's exec looks like from the socket: the old
// image goes away and a fresh one takes the same path.
func restartImage(t *testing.T, old *daemonImage, socket string, providers []string) *daemonImage {
	t.Helper()

	require.NoError(t, old.srv.Close())

	return startImage(t, socket, providers)
}

func bootIDOf(t *testing.T, socket string) string {
	t.Helper()

	client, err := ctl.Dial(context.Background(), socket)
	require.NoError(t, err)

	defer func() { _ = client.Close() }()

	st, err := client.Status(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, st.BootID)

	return st.BootID
}

func shortDaemonWait(t *testing.T, d time.Duration) {
	t.Helper()

	original := daemonWait
	daemonWait = d

	t.Cleanup(func() { daemonWait = original })
}

// captureOutput collects what the bootstrap printed. Both streams are the same
// conversation with the user, so they are read as one.
func captureOutput(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	require.NoError(t, err)

	stdout, stderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = w, w

	collected := make(chan string, 1)

	go func() {
		body, _ := io.ReadAll(r)
		collected <- string(body)
	}()

	fn()

	os.Stdout, os.Stderr = stdout, stderr

	require.NoError(t, w.Close())

	out := <-collected

	require.NoError(t, r.Close())

	return out
}
