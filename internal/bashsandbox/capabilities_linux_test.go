//go:build linux

package bashsandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/pilat/coagent/internal/procexec"
)

func TestBubblewrapAmbientCapabilities(t *testing.T) {
	if os.Getenv("COAGENT_AMBIENT_FIXTURE") != "1" {
		if os.Getenv("CI") != "true" {
			t.Skip("privileged launcher scenario runs in CI")
		}
		require.NotZero(t, os.Getuid(), "outer test must run as the ordinary CI user")
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		//nolint:gosec // Fixed argv; re-invokes this test binary.
		command := exec.CommandContext(ctx, "sudo", "-n", "--", "/usr/bin/setpriv",
			"--reuid="+strconv.Itoa(os.Getuid()), "--regid="+strconv.Itoa(os.Getgid()), "--clear-groups",
			"--inh-caps=+net_admin,+sys_admin", "--ambient-caps=+net_admin,+sys_admin",
			"env", "CI=true", "COAGENT_AMBIENT_FIXTURE=1", os.Args[0],
			"-test.run=^TestBubblewrapAmbientCapabilities$", "-test.timeout=20s", "-test.v")
		output, err := command.CombinedOutput()
		require.NoError(t, err, "%s", output)
		return
	}
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var before, after [2]unix.CapUserData
	require.NoError(t, unix.Capget(&header, &before[0]))
	require.NotZero(t, before[0].Effective)
	home := testSandboxHome(t)
	project := testDir(t, filepath.Join(home, "project"))
	policy := testProcessPolicy(t, testCompiledPolicy(t, project, false))
	runner, err := newEnabledRunner(policy)
	require.NoError(t, err)
	command, err := runner.BashCommand(
		t.Context(),
		"while read -r key value; do case $key in CapEff:|CapPrm:|CapInh:|CapAmb:) test \"$value\" = 0000000000000000 || exit 9;; esac; done < /proc/self/status",
		project,
	)
	require.NoError(t, err)
	defer procexec.CloseExtraFiles(command)
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
	require.NoError(t, unix.Capget(&header, &after[0]))
	require.Equal(t, before, after, "orchestrator retains its network administration capabilities")
}
