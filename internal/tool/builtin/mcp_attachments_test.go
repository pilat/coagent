package builtin

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/mcp"
	"github.com/pilat/coagent/internal/safefile"
	"github.com/pilat/coagent/internal/sandboxpolicy"
)

func TestMCPAttachmentSinkStoresAuthorizedImage(t *testing.T) {
	t.Cleanup(coagenthome.Override(t.TempDir()))
	project := t.TempDir()
	root, err := coagenthome.ProcessProjectDir(9)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(root, 0o700))
	imageBytes := testMCPPNG(t)
	for _, confined := range []bool{false, true} {
		t.Run(map[bool]string{false: "host", true: "sandbox"}[confined], func(t *testing.T) {
			policy := sandboxpolicy.Policy{}
			if confined {
				policy, err = sandboxpolicy.Compile(sandboxpolicy.Request{
					ProjectRoot: project, WorkDir: project, ProjectID: 9, ProcessOutputRoot: root,
				})
				require.NoError(t, err)
			}
			access, openErr := safefile.New(policy, project)
			require.NoError(t, openErr)
			defer func() { require.NoError(t, access.Close()) }()
			sink := &mcpAttachmentSink{projectID: 9, sessionID: 17, access: access}
			attachment, storeErr := sink.Store(mcp.BinaryPart{Kind: "image", MIME: "image/png", Data: imageBytes})
			require.NoError(t, storeErr)
			require.NotNil(t, attachment.Image)
			assert.Equal(t, 2, attachment.Image.Width)
			assert.Equal(t, 1, attachment.Image.Height)
			assert.NotEmpty(t, attachment.Image.Digest)
			assert.NotContains(t, attachment.Note, attachment.Image.Path)
			assert.True(
				t,
				strings.HasPrefix(attachment.Image.Path, filepath.Join(root, coagenthome.MCPAttachmentsDirName)),
			)
			info, statErr := os.Stat(attachment.Image.Path)
			require.NoError(t, statErr)
			assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
			assert.NoError(t, access.AuthorizeRead(attachment.Image.Path, attachment.Image.ReadRootID))
		})
	}
}

func TestMCPAttachmentSinkRejectsUnsupportedAndOversizedImages(t *testing.T) {
	t.Cleanup(coagenthome.Override(t.TempDir()))
	access, err := safefile.New(sandboxpolicy.Policy{}, t.TempDir())
	require.NoError(t, err)
	defer func() { require.NoError(t, access.Close()) }()
	sink := &mcpAttachmentSink{projectID: 2, sessionID: 3, access: access}
	for _, part := range []mcp.BinaryPart{
		{Kind: "image", MIME: "image/png", Data: []byte("bad")},
		{Kind: "image", MIME: "image/jpeg", Data: testMCPPNG(t)},
		{Kind: "image", MIME: "image/png", Data: append(testMCPPNG(t), make([]byte, maxImageBytes)...)},
		{Kind: "audio", MIME: "audio/wav", Data: []byte("sound")},
	} {
		attachment, storeErr := sink.Store(part)
		require.NoError(t, storeErr)
		assert.Nil(t, attachment.Image)
		assert.Contains(t, attachment.Note, "bytes")
		assert.Contains(t, attachment.Note, part.MIME)
	}
}

func testMCPPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 1))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var out bytes.Buffer
	require.NoError(t, png.Encode(&out, img))
	return out.Bytes()
}
