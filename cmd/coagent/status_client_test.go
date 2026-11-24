package main

import (
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStatusOf_OlderDaemonGetsTheRestartHint(t *testing.T) {
	socket := socketPath(t)
	var lc net.ListenConfig
	listener, err := lc.Listen(t.Context(), "unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		err = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if err == nil {
			_, err = io.WriteString(conn, "{\"app\":\"coagent\",\"binary_version\":\"0.9.0\",\"protocol_version\":1}\n")
		}
		if err == nil {
			_, err = io.Copy(io.Discard, conn)
		}
		done <- err
	}()

	var code int
	stderr := captureStatusStderr(t, func() { code = statusOf(t.Context(), socket) })
	assert.Equal(t, exitError, code)
	assert.True(
		t,
		strings.HasSuffix(
			stderr,
			"the daemon may be running an older coagent binary — `sudo coagent restart` to apply this one\n",
		),
	)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(6 * time.Second):
		t.Fatal("old daemon connection did not close")
	}
}

func TestStatusOf_BoundButSilentIsStarting(t *testing.T) {
	original := statusTimeout
	statusTimeout = 150 * time.Millisecond
	t.Cleanup(func() { statusTimeout = original })

	socket := socketPath(t)
	var lc net.ListenConfig
	listener, err := lc.Listen(t.Context(), "unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	var code int
	stdout := captureStatusStdout(t, func() { code = statusOf(t.Context(), socket) })
	assert.Equal(t, exitNotRunning, code)
	assert.Equal(t, "daemon starting — not answering yet\n", stdout)
}

func captureStatusStderr(t *testing.T, render func()) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	original := os.Stderr
	t.Cleanup(func() {
		os.Stderr = original
		_ = reader.Close()
		_ = writer.Close()
	})
	os.Stderr = writer
	render()
	require.NoError(t, writer.Close())
	os.Stderr = original
	output, err := io.ReadAll(reader)
	require.NoError(t, err)

	return string(output)
}
