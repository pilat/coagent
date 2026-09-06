package llm

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/safefile"
)

func TestResolveImage_RootedReferenceRejectsLaterCanonicalPathEscape(t *testing.T) {
	base := t.TempDir()
	project := filepath.Join(base, "project")
	require.NoError(t, os.Mkdir(project, 0o755))
	inside := filepath.Join(project, "inside.png")
	outside := filepath.Join(base, "outside.png")
	require.NoError(t, os.WriteFile(inside, []byte("inside pixels"), 0o600))
	require.NoError(t, os.WriteFile(outside, []byte("outside secret"), 0o600))
	link := filepath.Join(project, "image.png")
	require.NoError(t, os.Symlink(inside, link))

	access, err := safefile.New(project, safefile.ProjectConfined)
	require.NoError(t, err)
	path, err := access.Resolve(link)
	require.NoError(t, err)
	require.NoError(t, access.Close())
	require.NoError(t, os.Remove(inside))
	require.NoError(t, os.Symlink(outside, inside))

	data, reason := resolveImage([]string{"image"}, llmwire.ImageRef{
		Path: path.Canonical, ReadRoot: path.ReadRoot, ReadRootID: path.ReadRootID,
		Mime: llmwire.MimeImagePng,
	}, zap.NewNop())
	assert.Nil(t, data)
	assert.Equal(t, llmwire.ImageOmitReasonUnreadable, reason)
}

func TestResolveImage_RootedReferenceRejectsReplacedRoot(t *testing.T) {
	base := t.TempDir()
	project := filepath.Join(base, "project")
	require.NoError(t, os.Mkdir(project, 0o755))
	inside := filepath.Join(project, "image.png")
	require.NoError(t, os.WriteFile(inside, []byte("inside pixels"), 0o600))

	access, err := safefile.New(project, safefile.ProjectConfined)
	require.NoError(t, err)
	path, err := access.Resolve(inside)
	require.NoError(t, err)
	require.NoError(t, access.Close())

	renamed := filepath.Join(base, "renamed-project")
	require.NoError(t, os.Rename(project, renamed))
	outside := filepath.Join(base, "outside")
	require.NoError(t, os.Mkdir(outside, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "image.png"), []byte("outside secret"), 0o600))
	require.NoError(t, os.Symlink(outside, project))

	data, reason := resolveImage([]string{"image"}, llmwire.ImageRef{
		Path: path.Canonical, ReadRoot: path.ReadRoot, ReadRootID: path.ReadRootID,
		Mime: llmwire.MimeImagePng,
	}, zap.NewNop())
	assert.Nil(t, data)
	assert.Equal(t, llmwire.ImageOmitReasonUnreadable, reason)
}

func TestResolveImage_UnconfinedHistoricalReferenceRemainsReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "historical.png")
	require.NoError(t, os.WriteFile(path, []byte("accepted pixels"), 0o600))

	data, reason := resolveImage([]string{"image"}, llmwire.ImageRef{
		Path: path, Mime: llmwire.MimeImagePng,
	}, zap.NewNop())
	assert.Empty(t, reason)
	assert.Equal(t, "accepted pixels", string(data))
}
