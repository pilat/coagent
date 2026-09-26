package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// opaqueFixtureMode makes this binary stand in for a same-user service that
// hides itself from inspection, the way a credential agent does.
const opaqueFixtureMode = "__coagent_opaque_fixture"

func runOpaqueFixture(args []string) (bool, error) {
	if len(args) != 2 || args[0] != opaqueFixtureMode {
		return false, nil
	}

	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return true, err
	}

	if err := os.WriteFile(args[1], []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		return true, err
	}

	time.Sleep(2 * time.Minute)

	return true, nil
}

func TestHasAncestor_FindsTheDaemonAboveItsOwnChild(t *testing.T) {
	tree := map[int]int{4242: 99, 99: 7, 7: 1}
	parent := func(pid int) (int, bool) { p, ok := tree[pid]; return p, ok }

	assert.True(t, hasAncestor(4242, 7, parent), "a grandchild of the daemon is its workload")
	assert.True(t, hasAncestor(99, 7, parent))
}

func TestHasAncestor_StopsAtTheProcessTreeRoot(t *testing.T) {
	tree := map[int]int{4242: 99, 99: 1}
	parent := func(pid int) (int, bool) { p, ok := tree[pid]; return p, ok }

	assert.False(t, hasAncestor(4242, 7, parent), "an unrelated process is not the daemon's")
}

func TestHasAncestor_ToleratesAnUnreadableOrVanishedParent(t *testing.T) {
	assert.False(t, hasAncestor(4242, 7, func(int) (int, bool) { return 0, false }))
	assert.False(t, hasAncestor(4242, 7, func(int) (int, bool) { return 0, true }))
}

// A reused identifier can turn the parent chain into a cycle; the walk must end.
func TestHasAncestor_BoundsACyclicParentChain(t *testing.T) {
	assert.False(t, hasAncestor(4242, 7, func(pid int) (int, bool) { return pid, true }))
}

func TestProcessParent_ReadsThroughACommandContainingBrackets(t *testing.T) {
	parent, ok := processParent(os.Getpid())
	require.True(t, ok)
	assert.Equal(t, os.Getppid(), parent)
}

func TestNamespaceHasWorkloads_ReportsNothingForAVanishedHolder(t *testing.T) {
	assert.False(t, namespaceHasWorkloads(-1), "a holder that is gone has no workloads")
}

// TestNamespaceHasWorkloads_IgnoresAnOpaqueBystander drives the real probe
// against a real namespace. The retirement unit test fakes this seam, so a
// generation leaked forever while it stayed green: an unrelated same-user
// process that hides itself from inspection was counted as a workload.
func TestNamespaceHasWorkloads_IgnoresAnOpaqueBystander(t *testing.T) {
	startOpaqueBystander(t)

	holder := holderNamespace(t, []string{"sleep", "120"})
	assert.False(t, namespaceHasWorkloads(holder),
		"a process the daemon did not launch is no evidence of a workload")

	busy := holderNamespace(t, []string{"sh", "-c", "sleep 120 & exec sleep 120"})
	require.Eventually(t, func() bool { return namespaceHasWorkloads(busy) },
		10*time.Second, 100*time.Millisecond,
		"a process sharing the namespace must still keep the generation alive")
}

// startOpaqueBystander leaves a same-user process the test did not parent: it
// reparents to init, so the probe cannot excuse it as one of its own children.
func startOpaqueBystander(t *testing.T) {
	t.Helper()

	self, err := os.Executable()
	require.NoError(t, err)

	pidFile := filepath.Join(t.TempDir(), "opaque.pid")
	launch := strings.Join([]string{self, opaqueFixtureMode, pidFile, "&"}, " ")
	require.NoError(t, exec.CommandContext(t.Context(), "sh", "-c", launch).Run())

	var pid int

	require.Eventually(t, func() bool {
		raw, readErr := os.ReadFile(pidFile)
		if readErr != nil {
			return false
		}

		pid, readErr = strconv.Atoi(strings.TrimSpace(string(raw)))

		return readErr == nil
	}, 10*time.Second, 50*time.Millisecond, "the opaque bystander never reported itself")

	t.Cleanup(func() { _ = unix.Kill(pid, unix.SIGKILL) })

	_, err = os.Readlink("/proc/" + strconv.Itoa(pid) + "/ns/net")
	require.ErrorIs(t, err, os.ErrPermission, "the bystander must be opaque, or this proves nothing")
	require.False(t, hasAncestor(pid, os.Getpid(), processParent), "the bystander must not be our child")
}

// holderNamespace starts a process holding its own network namespace and
// returns its identifier, standing in for a generation's setup child. The idle
// case must be a single process: a shell that forks would itself be a workload.
func holderNamespace(t *testing.T, argv []string) int {
	t.Helper()

	command := exec.CommandContext(t.Context(), "unshare",
		append([]string{"--user", "--map-root-user", "--net", "--"}, argv...)...)
	if err := command.Start(); err != nil {
		t.Skipf("this environment cannot create a user namespace: %v", err)
	}

	t.Cleanup(func() {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
	})

	// unshare(2) runs after the process starts, so the descriptor answers with
	// our own namespace until it does. Waiting for a readable link would pass
	// immediately and leave the probe scanning the whole host.
	ours, err := os.Readlink("/proc/self/ns/net")
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		held, readErr := os.Readlink("/proc/" + strconv.Itoa(command.Process.Pid) + "/ns/net")

		return readErr == nil && held != ours
	}, 10*time.Second, 50*time.Millisecond, "the holder never entered a namespace of its own")

	return command.Process.Pid
}
