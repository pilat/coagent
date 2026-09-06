package safefile

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

	access, err := New(project, ProjectConfined)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, access.Close()) })

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

	access, err := New(project, ProjectConfined)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, access.Close()) })
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
	project := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	require.NoError(t, os.WriteFile(outside, []byte("host"), 0o600))

	access, err := New(project, HostReadable)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, access.Close()) })
	opened, err := access.Open(outside)
	require.NoError(t, err)
	content, err := io.ReadAll(opened.File)
	require.NoError(t, err)
	require.NoError(t, opened.File.Close())
	assert.Equal(t, "host", string(content))
}
