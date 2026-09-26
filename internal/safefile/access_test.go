//go:build linux

package safefile

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sandboxpolicy"
)

// grantedPolicy admits the project read-write plus any additional grants, the
// same shape a session's compiled policy has.
func grantedPolicy(project string, grants ...sandboxpolicy.Grant) sandboxpolicy.Policy {
	base := []sandboxpolicy.Grant{{
		Source: project, Target: project, Mode: sandboxpolicy.ModeReadWrite,
		Kind: sandboxpolicy.KindDir, Present: true,
	}}

	return sandboxpolicy.Policy{
		ProjectRoot: project, WorkDir: project, Grants: append(base, grants...),
	}
}

func newGrantedAccess(t *testing.T, policy sandboxpolicy.Policy) Access {
	t.Helper()

	access, err := New(policy, policy.WorkDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, access.Close()) })

	return access
}

func TestProjectAccessAllowsInsideAndRejectsEscapes(t *testing.T) {
	base := t.TempDir()
	project := filepath.Join(base, "project")
	require.NoError(t, os.Mkdir(project, 0o755))

	inside := filepath.Join(project, "inside.txt")
	outside := filepath.Join(base, "outside.txt")
	require.NoError(t, os.WriteFile(inside, []byte("inside"), 0o600))
	require.NoError(t, os.WriteFile(outside, []byte("outside"), 0o600))
	require.NoError(t, os.Symlink(inside, filepath.Join(project, "inside-link")))
	require.NoError(t, os.Symlink(outside, filepath.Join(project, "outside-link")))

	access := newGrantedAccess(t, grantedPolicy(project))

	for _, name := range []string{"inside.txt", "inside-link", inside} {
		opened, err := access.Open(name)
		require.NoError(t, err, name)

		content, err := io.ReadAll(opened.File)
		require.NoError(t, err)
		require.NoError(t, opened.File.Close())
		assert.Equal(t, "inside", string(content))
		assert.Equal(t, access.CanonicalRoot(), opened.Path.ReadRoot)
	}

	for _, name := range []string{"../outside.txt", outside, "outside-link"} {
		_, err := access.Open(name)
		require.Error(t, err, name)
		require.ErrorIs(t, err, ErrOutsideProject)
		assert.ErrorContains(t, err, ShieldDeniedMessage)
	}
}

func TestProjectAccessRootSurvivesAncestorRename(t *testing.T) {
	base := t.TempDir()
	project := filepath.Join(base, "project")
	require.NoError(t, os.Mkdir(project, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(project, "data"), []byte("before"), 0o600))

	access := newGrantedAccess(t, grantedPolicy(project))
	renamed := filepath.Join(base, "renamed")
	require.NoError(t, os.Rename(project, renamed))

	opened, err := access.Open("data")
	require.NoError(t, err)

	content, err := io.ReadAll(opened.File)
	require.NoError(t, err)
	require.NoError(t, opened.File.Close())
	assert.Equal(t, "before", string(content))
}

func TestHostAccessPreservesCrossProjectReads(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside")
	require.NoError(t, os.WriteFile(outside, []byte("host"), 0o600))

	access := newGrantedAccess(t, sandboxpolicy.Policy{})

	opened, err := access.Open(outside)
	require.NoError(t, err)

	content, err := io.ReadAll(opened.File)
	require.NoError(t, err)
	require.NoError(t, opened.File.Close())
	assert.Equal(t, "host", string(content))
	assert.Equal(t, HostReadable, access.Scope())
}

func TestConfinedAccessSeparatesReadOnlyFromWritableGrants(t *testing.T) {
	project := t.TempDir()
	readOnly := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(readOnly, "cache"), []byte("cached"), 0o600))

	policy := grantedPolicy(project, sandboxpolicy.Grant{
		Source: readOnly, Target: readOnly, Mode: sandboxpolicy.ModeReadOnly,
		Kind: sandboxpolicy.KindDir, Present: true,
	})
	access := newGrantedAccess(t, policy)

	opened, err := access.Open(filepath.Join(readOnly, "cache"))
	require.NoError(t, err)
	require.NoError(t, opened.File.Close())

	_, err = access.OpenFile(filepath.Join(readOnly, "new"), os.O_CREATE|os.O_WRONLY, 0o600)
	require.ErrorIs(t, err, ErrOutsideProject)

	_, err = access.OpenFile(filepath.Join(project, "new"), os.O_CREATE|os.O_WRONLY, 0o600)
	require.NoError(t, err)
}

func TestConfinedAccessMapsVirtualTempToProjectBacking(t *testing.T) {
	project := t.TempDir()
	backing := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(backing, "marker"), []byte("private"), 0o600))

	policy := grantedPolicy(project, sandboxpolicy.Grant{
		Source: backing, Target: sandboxpolicy.TempPath, Mode: sandboxpolicy.ModeReadWrite,
		Kind: sandboxpolicy.KindDir, Present: true,
	})
	access := newGrantedAccess(t, policy)

	path, err := access.Resolve("/tmp/marker")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(backing, "marker"), path.Canonical)
	assert.Equal(t, backing, path.ReadRoot)

	opened, err := access.Open("/tmp/marker")
	require.NoError(t, err)

	content, err := io.ReadAll(opened.File)
	require.NoError(t, err)
	require.NoError(t, opened.File.Close())
	assert.Equal(t, "private", string(content))
}

func TestConfinedAccessRequiresAnAdmittingGrant(t *testing.T) {
	project := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	other := filepath.Join(t.TempDir(), "other")
	require.NoError(t, os.WriteFile(other, []byte("other"), 0o600))

	access := newGrantedAccess(t, grantedPolicy(project))

	_, err := access.Resolve(other)
	require.ErrorIs(t, err, ErrOutsideProject)
	_, err = access.Resolve(filepath.Join(home, ".ssh", "id_ed25519"))
	require.ErrorIs(t, err, ErrOutsideProject)
}

func TestAuthorizeReadRequiresCurrentGrantAndIdentity(t *testing.T) {
	project := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(project, "data"), []byte("data"), 0o600))

	access := newGrantedAccess(t, grantedPolicy(project))

	path, err := access.Resolve("data")
	require.NoError(t, err)
	require.NoError(t, access.AuthorizeRead(path.Canonical, path.ReadRootID))

	require.ErrorIs(t, access.AuthorizeRead(path.Canonical, "1:2"), ErrOutsideProject)
	require.ErrorIs(
		t,
		access.AuthorizeRead(filepath.Join(t.TempDir(), "elsewhere"), path.ReadRootID),
		ErrOutsideProject,
	)
}

func TestConfinedAccessRefusesSingleFileGrantOutsideItsPath(t *testing.T) {
	project := t.TempDir()
	configDir := t.TempDir()
	configFile := filepath.Join(configDir, "config")
	require.NoError(t, os.WriteFile(configFile, []byte("value"), 0o600))

	policy := grantedPolicy(project, sandboxpolicy.Grant{
		Source: configFile, Target: configFile, Mode: sandboxpolicy.ModeReadOnly,
		Kind: sandboxpolicy.KindFile, Present: true,
	})
	access := newGrantedAccess(t, policy)

	opened, err := access.Open(configFile)
	require.NoError(t, err)
	require.NoError(t, opened.File.Close())
	assert.Equal(t, configFile, opened.Path.ReadRoot)

	_, err = access.Open(filepath.Join(configDir, "other"))
	require.ErrorIs(t, err, ErrOutsideProject)
}

func TestConfinedAccessRejectsReplacedSingleFileGrant(t *testing.T) {
	project := t.TempDir()
	configDir := t.TempDir()
	configFile := filepath.Join(configDir, "config")
	secret := filepath.Join(configDir, "secret")
	require.NoError(t, os.WriteFile(configFile, []byte("allowed"), 0o600))
	require.NoError(t, os.WriteFile(secret, []byte("secret"), 0o600))
	access := newGrantedAccess(t, grantedPolicy(project, sandboxpolicy.Grant{
		Source: configFile, Target: configFile, Mode: sandboxpolicy.ModeReadOnly,
		Kind: sandboxpolicy.KindFile, Present: true,
	}))

	require.NoError(t, os.Remove(configFile))
	require.NoError(t, os.Symlink(secret, configFile))
	_, err := access.Open(configFile)
	require.Error(t, err)
	_, _, err = access.Stat(configFile)
	require.Error(t, err)
}

func TestConfinedAccessRejectsReplacedGrantParent(t *testing.T) {
	project := t.TempDir()
	base := t.TempDir()
	parent := filepath.Join(base, "config")
	secretParent := filepath.Join(base, "secret")
	require.NoError(t, os.Mkdir(parent, 0o700))
	require.NoError(t, os.Mkdir(secretParent, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(parent, "settings"), []byte("allowed"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(secretParent, "settings"), []byte("secret"), 0o600))
	policy := grantedPolicy(project, sandboxpolicy.Grant{
		Source: filepath.Join(parent, "settings"), Target: filepath.Join(parent, "settings"),
		Mode: sandboxpolicy.ModeReadOnly, Kind: sandboxpolicy.KindFile, Present: true,
	})
	require.NoError(t, os.Rename(parent, filepath.Join(base, "old-config")))
	require.NoError(t, os.Symlink(secretParent, parent))
	_, err := New(policy, project)
	require.Error(t, err)
}

func TestConfinedAccessRejectsReplacedFileBeforeHoldingIt(t *testing.T) {
	project := t.TempDir()
	directory := t.TempDir()
	granted := filepath.Join(directory, "config")
	secret := filepath.Join(directory, "secret")
	require.NoError(t, os.WriteFile(granted, []byte("allowed"), 0o600))
	require.NoError(t, os.WriteFile(secret, []byte("secret"), 0o600))
	policy := grantedPolicy(project, sandboxpolicy.Grant{
		Source: granted, Target: granted, Mode: sandboxpolicy.ModeReadOnly,
		Kind: sandboxpolicy.KindFile, Present: true,
	})
	require.NoError(t, os.Remove(granted))
	require.NoError(t, os.Symlink(secret, granted))
	_, err := New(policy, project)
	require.Error(t, err)
}

func TestConfinedAccessOpensMaterializedOptionalDirectory(t *testing.T) {
	project := t.TempDir()
	root := filepath.Join(t.TempDir(), "new-cache")
	policy := grantedPolicy(project, sandboxpolicy.Grant{
		Source: root, Target: root, Mode: sandboxpolicy.ModeReadWrite,
		Kind: sandboxpolicy.KindDir, Present: false,
	})
	_, err := New(policy, project)
	require.ErrorContains(t, err, "was not materialized")
	require.NoError(t, os.Mkdir(root, 0o700))
	access := newGrantedAccess(t, policy)
	opened, err := access.OpenFile(filepath.Join(root, "result"), os.O_CREATE|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	require.NoError(t, opened.File.Close())
}
