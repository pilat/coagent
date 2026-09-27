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

func TestConfinedAccessReadsOutsideTheProject(t *testing.T) {
	project := t.TempDir()
	outside := filepath.Join(t.TempDir(), "note")
	require.NoError(t, os.WriteFile(outside, []byte("ordinary"), 0o600))

	access := newGrantedAccess(t, confinedPolicy(project))
	assert.Equal(t, ProjectConfined, access.Scope())

	opened, err := access.Open(outside)
	require.NoError(t, err)

	content, err := io.ReadAll(opened.File)
	require.NoError(t, err)
	require.NoError(t, opened.File.Close())
	assert.Equal(t, "ordinary", string(content))
}

// TestConfinedAccessKeepsWritesInsideTheEntryTable is the boundary that
// matters: these tools run in the daemon process, so the mount plan does not
// constrain them and this package is the only thing that does.
func TestConfinedAccessKeepsWritesInsideTheEntryTable(t *testing.T) {
	project := t.TempDir()
	outside := t.TempDir()

	access := newGrantedAccess(t, confinedPolicy(project))

	_, err := access.OpenFile(filepath.Join(project, "new"), os.O_CREATE|os.O_WRONLY, 0o600)
	require.NoError(t, err, "the project is granted read-write")

	_, err = access.OpenFile(filepath.Join(outside, "new"), os.O_CREATE|os.O_WRONLY, 0o600)
	require.ErrorIs(t, err, ErrOutsideProject,
		"a readable path is not a writable one")
	assert.NoFileExists(t, filepath.Join(outside, "new"))
}

func TestConfinedAccessRefusesADeniedDirectory(t *testing.T) {
	project := t.TempDir()
	secretDir := filepath.Join(t.TempDir(), "credentials")
	require.NoError(t, os.Mkdir(secretDir, 0o700))
	key := filepath.Join(secretDir, "id_ed25519")
	require.NoError(t, os.WriteFile(key, []byte("PRIVATE"), 0o600))

	access := newGrantedAccess(t, confinedPolicy(project, sandboxpolicy.Entry{
		Path: secretDir, Action: sandboxpolicy.ActionDeny, Kind: sandboxpolicy.KindDir, Present: true,
	}))

	_, err := access.Open(key)
	require.ErrorIs(t, err, ErrOutsideProject)

	_, err = access.Open(secretDir)
	require.ErrorIs(t, err, ErrOutsideProject)

	_, _, err = access.Stat(key)
	require.ErrorIs(t, err, ErrOutsideProject)

	_, _, err = access.ReadDir(secretDir)
	require.ErrorIs(t, err, ErrOutsideProject)
}

func TestConfinedAccessRefusesADeniedFileButNotItsSiblings(t *testing.T) {
	project := t.TempDir()
	home := t.TempDir()
	secret := filepath.Join(home, ".netrc")
	require.NoError(t, os.WriteFile(secret, []byte("password hunter2"), 0o600))
	sibling := filepath.Join(home, ".gitconfig")
	require.NoError(t, os.WriteFile(sibling, []byte("[user]"), 0o600))

	access := newGrantedAccess(t, confinedPolicy(project, sandboxpolicy.Entry{
		Path: secret, Action: sandboxpolicy.ActionDeny, Kind: sandboxpolicy.KindFile, Present: true,
	}))

	_, err := access.Open(secret)
	require.ErrorIs(t, err, ErrOutsideProject)

	opened, err := access.Open(sibling)
	require.NoError(t, err, "denying one file must not hide its siblings")
	require.NoError(t, opened.File.Close())
}

func TestConfinedAccessRefusesWritingThroughADeniedDirectory(t *testing.T) {
	project := t.TempDir()
	secretDir := filepath.Join(project, "credentials")
	require.NoError(t, os.Mkdir(secretDir, 0o700))

	// The deny sits inside the writable project: it must still win.
	access := newGrantedAccess(t, confinedPolicy(project, sandboxpolicy.Entry{
		Path: secretDir, Action: sandboxpolicy.ActionDeny, Kind: sandboxpolicy.KindDir, Present: true,
	}))

	_, err := access.OpenFile(filepath.Join(secretDir, "planted"), os.O_CREATE|os.O_WRONLY, 0o600)
	require.ErrorIs(t, err, ErrOutsideProject)
	assert.NoFileExists(t, filepath.Join(secretDir, "planted"))
}

func TestConfinedAccessRefusesWritingThroughSymlinkToDeniedDirectory(t *testing.T) {
	project := t.TempDir()
	secretDir := filepath.Join(project, "credentials")
	require.NoError(t, os.Mkdir(secretDir, 0o700))
	link := filepath.Join(project, "alias")
	require.NoError(t, os.Symlink("credentials", link))
	access := newGrantedAccess(t, confinedPolicy(project, sandboxpolicy.Entry{
		Path: secretDir, Action: sandboxpolicy.ActionDeny, Kind: sandboxpolicy.KindDir, Present: true,
	}))

	opened, err := access.OpenFile(filepath.Join(link, "planted"), os.O_CREATE|os.O_WRONLY, 0o600)
	if opened != nil {
		require.NoError(t, opened.File.Close())
	}
	require.ErrorIs(t, err, ErrOutsideProject)
	assert.NoFileExists(t, filepath.Join(secretDir, "planted"))
}

// TestConfinedAuthorizeReadRechecksTheDeny covers the deferred path: a read
// recorded before a deny existed must not be replayable through an old
// reference.
func TestConfinedAuthorizeReadRechecksTheDeny(t *testing.T) {
	project := t.TempDir()
	secret := filepath.Join(t.TempDir(), "vault")
	require.NoError(t, os.Mkdir(secret, 0o700))
	recorded := filepath.Join(secret, "token")

	open := newGrantedAccess(t, confinedPolicy(project))
	require.NoError(t, open.AuthorizeRead(recorded, ""))

	denied := newGrantedAccess(t, confinedPolicy(project, sandboxpolicy.Entry{
		Path: secret, Action: sandboxpolicy.ActionDeny, Kind: sandboxpolicy.KindDir, Present: true,
	}))
	require.ErrorIs(t, denied.AuthorizeRead(recorded, ""), ErrOutsideProject)
}

func TestConfinedDeferredReadRejectsReplacementSymlink(t *testing.T) {
	project := t.TempDir()
	outside := t.TempDir()
	visible := filepath.Join(outside, "image")
	secret := filepath.Join(outside, "secret")
	require.NoError(t, os.WriteFile(visible, []byte("visible"), 0o600))
	require.NoError(t, os.WriteFile(secret, []byte("private"), 0o600))
	access := newGrantedAccess(t, confinedPolicy(project, sandboxpolicy.Entry{
		Path: secret, Action: sandboxpolicy.ActionDeny, Kind: sandboxpolicy.KindFile, Present: true,
	}))
	path, err := access.Resolve(visible)
	require.NoError(t, err)
	require.NoError(t, os.Remove(visible))
	require.NoError(t, os.Symlink(secret, visible))
	require.ErrorIs(t, access.AuthorizeRead(path.Canonical, path.ReadRootID), ErrOutsideProject)
	_, err = ReadFileAtRoot(path.ReadRoot, path.ReadRootID, path.Canonical)
	require.Error(t, err)
}

// TestDisabledSandboxKeepsUnrestrictedWrites pins the operator's explicit
// opt-out: a policy with no entries withholds nothing.
func TestDisabledSandboxKeepsUnrestrictedWrites(t *testing.T) {
	outside := t.TempDir()

	access := newGrantedAccess(t, sandboxpolicy.Policy{})
	assert.Equal(t, HostReadable, access.Scope())

	target := filepath.Join(outside, "written")
	opened, err := access.OpenFile(target, os.O_CREATE|os.O_WRONLY, 0o600)
	require.NoError(t, err, "the sandbox opt-out keeps writes unrestricted")
	require.NoError(t, opened.File.Close())
	assert.FileExists(t, target)
}
