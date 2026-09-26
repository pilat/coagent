//go:build linux

package procexec

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnprivileged_LeavesACapabilitylessProcessUntouched(t *testing.T) {
	command := exec.CommandContext(context.Background(), "/bin/echo", "hello")

	Unprivileged(command)

	require.NoError(t, command.Err)
	assert.Equal(t, "/bin/echo", command.Path)
	assert.Equal(t, []string{"/bin/echo", "hello"}, command.Args)
}

func TestRelaunchUnprivileged_RunsTheWorkloadThroughTheLauncher(t *testing.T) {
	if _, err := os.Stat(CapabilityLauncher); err != nil {
		t.Skipf("no capability launcher installed: %v", err)
	}

	command := exec.CommandContext(context.Background(), "/bin/echo", "hello")
	command.Env = []string{"PATH=/usr/bin"}

	require.NoError(t, relaunchUnprivileged(command, CapabilityLauncher))
	assert.Equal(t, CapabilityLauncher, command.Path)
	assert.Equal(
		t,
		[]string{CapabilityLauncher, "--ambient-caps=-all", "--inh-caps=-all", "--", "/bin/echo", "hello"},
		command.Args,
	)
}

// A launcher the workload's own user can replace would be the escalation it is
// supposed to prevent.
func TestRelaunchUnprivileged_RefusesALauncherThisUserCouldReplace(t *testing.T) {
	launcher := filepath.Join(t.TempDir(), "setpriv")
	require.NoError(t, os.WriteFile(launcher, []byte("#!/bin/sh\n"), 0o755))

	command := exec.CommandContext(context.Background(), "/bin/echo", "hello")
	command.Env = []string{"PATH=/usr/bin"}

	require.Error(t, relaunchUnprivileged(command, launcher))
	assert.Equal(t, "/bin/echo", command.Path, "a refused launch must not be rewritten")
}

func TestRelaunchUnprivileged_RefusesAMissingLauncher(t *testing.T) {
	command := exec.CommandContext(context.Background(), "/bin/echo", "hello")
	command.Env = []string{"PATH=/usr/bin"}

	require.Error(t, relaunchUnprivileged(command, filepath.Join(t.TempDir(), "absent")))
}

// The loader runs before the launcher can drop anything, so an override would
// execute with the orchestrator's rights.
func TestRelaunchUnprivileged_RefusesLoaderOverrides(t *testing.T) {
	if _, err := os.Stat(CapabilityLauncher); err != nil {
		t.Skipf("no capability launcher installed: %v", err)
	}

	for _, entry := range []string{"LD_PRELOAD=/tmp/evil.so", "LD_LIBRARY_PATH=/tmp", "GCONV_PATH=/tmp", "GLIBC_TUNABLES=x"} {
		t.Run(entry, func(t *testing.T) {
			command := exec.CommandContext(context.Background(), "/bin/echo", "hello")
			command.Env = []string{"PATH=/usr/bin", entry}

			require.Error(t, relaunchUnprivileged(command, CapabilityLauncher))
			assert.Equal(t, "/bin/echo", command.Path)
		})
	}
}

func TestRelaunchUnprivileged_AllowsAnEmptyLoaderVariable(t *testing.T) {
	if _, err := os.Stat(CapabilityLauncher); err != nil {
		t.Skipf("no capability launcher installed: %v", err)
	}

	command := exec.CommandContext(context.Background(), "/bin/echo", "hello")
	command.Env = []string{"PATH=/usr/bin", "LD_PRELOAD="}

	require.NoError(t, relaunchUnprivileged(command, CapabilityLauncher))
}

// A second pass must not stack launchers on top of each other.
func TestRestrictCapabilities_DoesNotWrapTheLauncherAgain(t *testing.T) {
	command := exec.CommandContext(context.Background(), CapabilityLauncher, "--", "/bin/echo")

	require.NoError(t, restrictCapabilities(command))
	assert.Equal(t, []string{CapabilityLauncher, "--", "/bin/echo"}, command.Args)
}

func TestUnprivileged_KeepsAnEarlierCommandError(t *testing.T) {
	command := exec.CommandContext(context.Background(), "definitely-not-on-path-\x00")

	require.Error(t, command.Err)
	before := command.Err

	Unprivileged(command)
	require.ErrorIs(t, command.Err, before)
}

// HoldsCapabilities is the seam Bubblewrap's own launcher check shares, so a
// wrong answer would silently stop stripping capabilities there too.
func TestHoldsCapabilities_ReportsNoneForAnOrdinaryProcess(t *testing.T) {
	holds, err := HoldsCapabilities()
	require.NoError(t, err)
	assert.False(t, holds, "a test process carries no capabilities to strip")
}

func TestLauncherArgv_ClearsBothSetsBeforeTheProgram(t *testing.T) {
	assert.Equal(t,
		[]string{"/l", "--ambient-caps=-all", "--inh-caps=-all", "--", "bwrap", "--die-with-parent"},
		LauncherArgv("/l", []string{"bwrap", "--die-with-parent"}))
}
