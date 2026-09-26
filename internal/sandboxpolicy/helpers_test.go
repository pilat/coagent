package sandboxpolicy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
)

// testHome isolates HOME and the environment names the shipped catalog
// resolves, so catalog paths are deterministic.
func testHome(t *testing.T) string {
	t.Helper()

	home := t.TempDir()
	t.Cleanup(coagenthome.Override(home))

	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Setenv("GH_CONFIG_DIR", filepath.Join(home, ".config", "gh"))
	t.Setenv("SSH_AUTH_SOCK", filepath.Join(home, "agent.sock"))

	return home
}

func testDir(t *testing.T, path string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(path, 0o700))

	return path
}

func testProject(t *testing.T, home string) string {
	t.Helper()

	return testDir(t, filepath.Join(home, "project"))
}

// testFile creates the parent directories and one regular file at path.
func testFile(t *testing.T, path string) string {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("fixture"), 0o600))

	return path
}

func testRequest(t *testing.T, project string) Request {
	t.Helper()

	tempRoot, err := coagenthome.SandboxTempDir("project-1")
	require.NoError(t, err)

	return Request{ProjectRoot: project, ProjectID: 1, WorkDir: project, TempRoot: tempRoot}
}

func testCatalog(t *testing.T, overrides map[string]Profile) Catalog {
	t.Helper()

	catalog, err := Load(overrides)
	require.NoError(t, err)

	return catalog
}

func grantFor(policy Policy, target string) (Grant, bool) {
	for _, grant := range policy.Grants {
		if grant.Target == target {
			return grant, true
		}
	}

	return Grant{}, false
}

func socketFor(policy Policy, path string) (SocketGrant, bool) {
	for _, socket := range policy.Sockets {
		if socket.Path == path {
			return socket, true
		}
	}

	return SocketGrant{}, false
}

func networkFor(policy Policy, address string) (Network, bool) {
	for _, entry := range policy.Network {
		if entry.Address == address {
			return entry, true
		}
	}

	return Network{}, false
}

func tempRootFor(t *testing.T, identity string) string {
	t.Helper()

	root, err := coagenthome.SandboxTempDir(identity)
	require.NoError(t, err)

	return root
}

func coagenthomeDir(t *testing.T) (string, error) {
	t.Helper()

	return coagenthome.Dir()
}

func symlink(target, link string) error {
	return os.Symlink(target, link)
}
