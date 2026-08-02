package install

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderUnit(t *testing.T) {
	_, got, err := expectedUnit(target{name: "testuser", home: "/home/testuser"})
	require.NoError(t, err)

	assert.Equal(t, golden(t, "coagent.service"), got)
}

func TestRenderPlist(t *testing.T) {
	_, got, err := expectedPlist(target{name: "testuser", home: "/Users/testuser"})
	require.NoError(t, err)

	assert.Equal(t, golden(t, "com.pilat.coagent.plist"), got)
}

// TestInstallBinaryReplacesRunning covers the reinstall-over-running case: the
// copy goes through a rename, so the destination's old inode stays alive and the
// write never hits ETXTBSY.
func TestInstallBinaryReplacesRunning(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "sub", "coagent")

	require.NoError(t, os.MkdirAll(filepath.Dir(dst), 0o755))
	require.NoError(t, os.WriteFile(dst, []byte("old"), 0o755))

	require.NoError(t, installBinary(dst, target{}))

	self, err := os.Executable()
	require.NoError(t, err)

	want, err := os.ReadFile(self)
	require.NoError(t, err)

	got, err := os.ReadFile(dst)
	require.NoError(t, err)

	assert.Equal(t, want, got)

	info, err := os.Stat(dst)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(binaryMode), info.Mode().Perm())

	entries, err := os.ReadDir(filepath.Dir(dst))
	require.NoError(t, err)
	assert.Len(t, entries, 1, "the temp file must not survive the rename")
}

func TestWriteFileAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "units", "coagent.service")

	require.NoError(t, writeFileAtomic(path, "first"))
	require.NoError(t, writeFileAtomic(path, "second"))

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "second", string(content))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(unitMode), info.Mode().Perm())
}

// TestResolveTargetPrefersSudoUser pins the rule that makes install correct
// under sudo: the service runs as the invoking account, not as root.
func TestResolveTargetPrefersSudoUser(t *testing.T) {
	t.Setenv("SUDO_USER", "root")

	got, err := resolveTarget()
	require.NoError(t, err)

	assert.Equal(t, "root", got.name)
	assert.NotEmpty(t, got.home)
}

func golden(t *testing.T, name string) string {
	t.Helper()

	content, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)

	return string(content)
}

// TestResolveTargetRefusesBareRoot is the safety branch: the daemon owns
// ~/.coagent, and root's copy of it is not the one anybody configured. Without
// SUDO_USER there is no login account to name in the unit, so install refuses
// rather than silently registering a service that runs as root.
func TestResolveTargetRefusesBareRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("only reachable when the process really is root")
	}

	t.Setenv("SUDO_USER", "")

	_, err := resolveTarget()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "sudo")
}

// TestResolveTargetFallsBackToTheCaller covers the sudo-free path: an update
// runs as whoever invoked it, so the caller is the target.
func TestResolveTargetFallsBackToTheCaller(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root takes the refusal branch, covered above")
	}

	t.Setenv("SUDO_USER", "")

	got, err := resolveTarget()

	require.NoError(t, err)
	assert.NotEmpty(t, got.name)
	assert.NotEmpty(t, got.home)
}

// The load-bearing half of the sudo-free update: a root install must hand the
// binary and the directories it created to the target.
func TestInstallBinaryChownsWhatItCreated(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "home")
	require.NoError(t, os.Mkdir(existing, 0o755))

	dst := filepath.Join(existing, ".local", "bin", "coagent")

	got := recordChown(t, 0)

	require.NoError(t, installBinary(dst, target{name: "testuser", uid: 501, gid: 20}))

	assert.Equal(t, []chownCall{
		{path: filepath.Join(existing, ".local"), uid: 501, gid: 20},
		{path: filepath.Join(existing, ".local", "bin"), uid: 501, gid: 20},
		{path: dst, uid: 501, gid: 20},
	}, *got, "a pre-existing directory keeps its owner")
}

// TestInstallBinaryDoesNotChownAsUser: a plain-user install already owns
// everything it wrote, and chown to another uid would fail anyway.
func TestInstallBinaryDoesNotChownAsUser(t *testing.T) {
	dst := filepath.Join(t.TempDir(), ".local", "bin", "coagent")

	got := recordChown(t, 1000)

	require.NoError(t, installBinary(dst, target{name: "testuser", uid: 501, gid: 20}))

	assert.Empty(t, *got)
}

// TestUnitStaleWhenMissing: nothing on disk cannot match what this version would
// write, so the update path warns rather than staying quiet.
func TestUnitStaleWhenMissing(t *testing.T) {
	stale, err := unitFileStale(filepath.Join(t.TempDir(), "missing.service"), "expected")
	require.NoError(t, err)
	assert.True(t, stale)
}

func TestUnitFileStaleComparesContents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "coagent.service")
	require.NoError(t, os.WriteFile(path, []byte("expected"), 0o600))

	stale, err := unitFileStale(path, "expected")
	require.NoError(t, err)
	assert.False(t, stale)

	stale, err = unitFileStale(path, "changed")
	require.NoError(t, err)
	assert.True(t, stale)
}

type chownCall struct {
	path string
	uid  int
	gid  int
}

// recordChown swaps both ownership seams for one test: chown is unobservable
// without root, and the root branch is unreachable in a normal test process.
func recordChown(t *testing.T, euid int) *[]chownCall {
	t.Helper()

	var calls []chownCall

	originalChown, originalEuid := chownFn, geteuid

	chownFn = func(path string, uid, gid int) error {
		calls = append(calls, chownCall{path: path, uid: uid, gid: gid})

		return nil
	}
	geteuid = func() int { return euid }

	t.Cleanup(func() { chownFn, geteuid = originalChown, originalEuid })

	return &calls
}
