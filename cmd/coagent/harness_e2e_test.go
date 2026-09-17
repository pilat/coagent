//go:build integration

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/ctl"
)

// The compiled daemon answers greeting, readiness, and status over the Unix
// socket with no config file and no terminal chat involved.
func TestHarnessE2E_DaemonAnswersGreetingReadinessAndStatus(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "coa-harness")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(home) })

	binary := buildBinary(t)
	socket := filepath.Join(home, coagenthome.DirName, coagenthome.SocketFileName)
	process := startDaemon(t, binary, home)
	defer func() { _ = process.Process.Kill() }()

	st := waitForStatus(t, socket, func(status ctl.StatusResult) bool { return true })
	assert.False(t, st.ConfigPresent, "no config file was provided")

	client, err := ctl.Dial(t.Context(), socket)
	require.NoError(t, err)
	defer func() { _ = client.Close() }()

	greeting := client.Greeting()
	assert.Equal(t, ctl.AppName, greeting.App)
	assert.Equal(t, ctl.ProtocolVersion, greeting.ProtocolVersion)

	// Every removed mutation is gone from the socket, not just unregistered.
	var ignored struct{}
	err = client.Call(t.Context(), "set_provider", nil, &ignored)
	require.ErrorContains(t, err, "unknown method")

	err = client.Call(t.Context(), "chat_open", nil, &ignored)
	require.ErrorContains(t, err, "unknown method")
}

func buildBinary(t *testing.T) string {
	t.Helper()

	out := filepath.Join(t.TempDir(), "coagent")
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Run())

	return out
}

func startDaemon(t *testing.T, binary, home string) *exec.Cmd {
	t.Helper()

	cmd := exec.Command(binary, "daemon")
	cmd.Env = isolatedProcessEnv(os.Environ(), home)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())

	t.Cleanup(func() { _ = cmd.Process.Kill() })

	return cmd
}

// waitForStatus polls until a daemon answers a status that satisfies want.
func waitForStatus(t *testing.T, socket string, want func(ctl.StatusResult) bool) ctl.StatusResult {
	t.Helper()

	deadline := time.Now().Add(2 * time.Minute)

	for time.Now().Before(deadline) {
		if st, ok := tryStatus(socket); ok && want(st) {
			return st
		}

		time.Sleep(100 * time.Millisecond)
	}

	t.Fatalf("no daemon answered on %s within the restart budget", socket)

	return ctl.StatusResult{}
}

func tryStatus(socket string) (ctl.StatusResult, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	c, err := ctl.Dial(ctx, socket)
	if err != nil {
		return ctl.StatusResult{}, false
	}

	defer func() { _ = c.Close() }()

	st, err := c.Status(ctx)

	return st, err == nil
}
