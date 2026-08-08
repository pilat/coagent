//go:build integration

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/ctl"
)

const smokeKey = "sk-ant-smoke-0000000000"

// TestApplyRestartsTheDaemon drives the whole restart pipeline against a real
// process: a bootstrap set_provider writes the secret and the config, answers
// over the socket, drains, and comes back on the new file as a new pid.
//
// It runs under -tags=integration because it compiles and forks the binary.
func TestApplyRestartsTheDaemon(t *testing.T) {
	// A unix socket path is capped near 100 bytes and a deep TMPDIR blows it.
	home, err := os.MkdirTemp("/tmp", "coa")
	require.NoError(t, err)

	t.Cleanup(func() { _ = os.RemoveAll(home) })

	binary := buildBinary(t)
	socket := filepath.Join(home, coagenthome.DirName, coagenthome.SocketFileName)

	proc := startDaemon(t, binary, home)

	before := waitForStatus(t, socket, func(st ctl.StatusResult) bool { return true })
	require.False(t, before.ConfigPresent, "the daemon starts with no config, as a first run does")

	client, err := ctl.Dial(context.Background(), socket)
	require.NoError(t, err)

	var v configops.Verdict

	err = client.Call(context.Background(), ctl.OpSetProvider, ctl.SetProviderParams{
		Name:   "work",
		Driver: "anthropic",
		APIKey: smokeKey,
	}, &v)
	require.NoError(t, err)
	require.True(t, v.Applied, v.Reason())

	_ = client.Close()

	// config_present is the unambiguous signal that the *new* image is answering:
	// the boot-time config is what status reports, and the old image had none.
	after := waitForStatus(t, socket, func(st ctl.StatusResult) bool { return st.ConfigPresent })
	assert.Equal(t, before.PID, after.PID, "exec keeps the pid; only the image changed")
	assert.NotEqual(t, before.BootID, after.BootID, "the boot id is what tells the new run from the draining one")
	require.NotEmpty(t, after.BootID)
	require.Len(t, after.Providers, 1)
	assert.Equal(t, "work", after.Providers[0].Name)
	assert.Equal(t, "anthropic", after.Providers[0].Driver)

	// The provider arrives with the model that makes it usable, enriched from the
	// live catalog — a provider alone would boot into a daemon no session can run
	// on, which is the state onboarding exists to avoid.
	assert.Equal(t, 1, after.ModelCount)
	assert.Equal(t, "claude-sonnet-5", after.DefaultModel)

	// The credential lives in the secrets file as a reference, never in the yaml.
	body, err := os.ReadFile(filepath.Join(home, coagenthome.DirName, coagenthome.ConfigFileName))
	require.NoError(t, err)
	assert.Contains(t, string(body), "${WORK_API_KEY}")
	assert.NotContains(t, string(body), "sk-ant-smoke")

	// The marker is cleared by the boot that consumed it.
	assert.NoFileExists(t, filepath.Join(home, coagenthome.DirName, coagenthome.PendingApplyFileName))

	require.NoError(t, proc.Process.Signal(syscall.SIGTERM))
}

// TestRestartOpPicksUpASwappedBinary is the sudo-free update end to end: the
// binary in place is replaced by a plain-user write, and the restart op alone —
// no systemctl, no sudo — brings the daemon back on it.
func TestRestartOpPicksUpASwappedBinary(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "coa")
	require.NoError(t, err)

	t.Cleanup(func() { _ = os.RemoveAll(home) })

	binary := buildStamped(t, "v9.9.9")
	socket := filepath.Join(home, coagenthome.DirName, coagenthome.SocketFileName)

	proc := startDaemon(t, binary, home)

	waitForStatus(t, socket, func(ctl.StatusResult) bool { return true })

	// The swap is what an update does: same path, new image, no privileges. The
	// daemon's captured exec path then holds the newer binary.
	newer, err := os.ReadFile(buildStamped(t, "v9.9.10"))
	require.NoError(t, err)
	require.NoError(t, os.Remove(binary))
	require.NoError(t, os.WriteFile(binary, newer, 0o755))

	client, err := ctl.Dial(context.Background(), socket)
	require.NoError(t, err)
	require.Equal(t, "v9.9.9", client.Greeting().BinaryVersion)

	var res ctl.RestartResult

	require.NoError(t, client.Call(context.Background(), ctl.OpRestartDaemon, nil, &res))
	assert.True(t, res.Restarting)

	_ = client.Close()

	waitForGreeting(t, socket, "v9.9.10")

	require.NoError(t, proc.Process.Signal(syscall.SIGTERM))
}

func buildBinary(t *testing.T) string {
	t.Helper()

	out := filepath.Join(t.TempDir(), "coagent")
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Run())

	return out
}

func buildStamped(t *testing.T, version string) string {
	t.Helper()

	out := filepath.Join(t.TempDir(), "coagent")
	cmd := exec.Command("go", "build",
		"-ldflags", "-X github.com/pilat/coagent/internal/version.Version="+version, "-o", out, ".")
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Run())

	return out
}

// waitForGreeting polls until a daemon announces the expected image.
func waitForGreeting(t *testing.T, socket, want string) {
	t.Helper()

	deadline := time.Now().Add(60 * time.Second)

	for time.Now().Before(deadline) {
		c, err := ctl.Dial(context.Background(), socket)
		if err == nil {
			got := c.Greeting().BinaryVersion

			_ = c.Close()

			if got == want {
				return
			}
		}

		time.Sleep(100 * time.Millisecond)
	}

	t.Fatalf("no daemon announced %s on %s within the restart budget", want, socket)
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

	deadline := time.Now().Add(60 * time.Second)

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
