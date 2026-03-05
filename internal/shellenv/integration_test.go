//go:build linux || darwin

package shellenv

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSnapshot_Integration_PerCwdActivation proves the core mechanism end to end:
// a real login+interactive bash, sourcing its rc, activates per-cwd state (the
// way mise/direnv key off the working directory), and the replayed snapshot
// reproduces it from a clean `env -i` base. Uses a synthetic HOME rc instead of a
// real version manager so it stays hermetic; gated because interactive shells
// behave unpredictably in some CI sandboxes.
func TestSnapshot_Integration_PerCwdActivation(t *testing.T) {
	if os.Getenv("COAGENT_SHELLENV_INTEGRATION") == "" {
		t.Skip("set COAGENT_SHELLENV_INTEGRATION=1 to run the interactive-shell integration test")
	}

	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed")
	}

	home := t.TempDir()
	// A login shell sources .bash_profile, which pulls in .bashrc (interactive).
	require.NoError(t, os.WriteFile(filepath.Join(home, ".bash_profile"),
		[]byte(". ~/.bashrc\n"), 0o600))
	// Per-cwd activation hook: export a marker from a cwd-local file.
	require.NoError(t, os.WriteFile(filepath.Join(home, ".bashrc"),
		[]byte("[ -f ./marker ] && export COAGENT_MARKER=\"$(cat ./marker)\"\n"), 0o600))

	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("SHELL", bash)

	p, ok := New().(*provider)
	require.True(t, ok)
	require.NotEmpty(t, p.Shell())

	check := func(marker string) string {
		wd := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(wd, "marker"), []byte(marker), 0o600))

		snap := p.Snapshot(context.Background(), wd)
		require.NotEmpty(t, snap)

		// Replay from a CLEAN env: source the snapshot, echo the captured marker.
		out, err := exec.Command("env", "-i", bash, "-c",
			"source "+shellQuote(snap)+"; printf '%s' \"$COAGENT_MARKER\"").Output()
		require.NoError(t, err)

		return string(out)
	}

	assert.Equal(t, "ALPHA", check("ALPHA"))
	assert.Equal(t, "BRAVO", check("BRAVO"))

	// A produced snapshot must source with empty stderr and exit 0 — proof the
	// readonly-exported filter and marker stripping are complete.
	wd := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(wd, "marker"), []byte("X"), 0o600))

	snap := p.Snapshot(context.Background(), wd)
	require.NotEmpty(t, snap)

	replay := exec.Command("env", "-i", bash, "--norc", "-c", "source "+shellQuote(snap))

	var stderr strings.Builder
	replay.Stderr = &stderr

	require.NoError(t, replay.Run(), "sourcing a snapshot must exit 0")
	assert.Empty(t, stderr.String(), "sourcing a snapshot must produce no stderr")
}

// TestWrapExec_Integration_ActivatesChildEnviron proves the LSP/MCP spawn path:
// a process launched via WrapExec (which exec-replaces the shell) inherits the
// per-cwd activated env, observable in its /proc/<pid>/environ — the exact check
// the plan prescribes for gopls/MCP children.
func TestWrapExec_Integration_ActivatesChildEnviron(t *testing.T) {
	if os.Getenv("COAGENT_SHELLENV_INTEGRATION") == "" {
		t.Skip("set COAGENT_SHELLENV_INTEGRATION=1 to run the interactive-shell integration test")
	}

	if runtime.GOOS != "linux" {
		t.Skip("/proc/<pid>/environ inspection is linux-only")
	}

	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed")
	}

	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, ".bash_profile"), []byte(". ~/.bashrc\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".bashrc"),
		[]byte("[ -f ./marker ] && export COAGENT_MARKER=\"$(cat ./marker)\"\n"), 0o600))

	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("SHELL", bash)

	provider := New()
	t.Cleanup(func() { _ = provider.Close() })

	wd := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(wd, "marker"), []byte("PINNED"), 0o600))

	cmd, err := provider.WrapExec(context.Background(), wd, []string{"sleep", "30"}, []string{"EXTRA=set"})
	require.NoError(t, err)
	require.NoError(t, cmd.Start())

	t.Cleanup(func() { _ = cmd.Process.Kill() })

	// `exec <argv>` is a second execve; poll until it replaces the shell so
	// /proc/environ reflects the server, not the mid-source shell.
	var got string

	require.Eventually(t, func() bool {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", cmd.Process.Pid))
		if err != nil {
			return false
		}

		got = string(b)

		return strings.Contains(got, "COAGENT_MARKER=PINNED")
	}, 5*time.Second, 20*time.Millisecond, "child must inherit the per-cwd activated env")

	assert.Contains(t, got, "EXTRA=set", "child must inherit non-colliding extraEnv")
}
