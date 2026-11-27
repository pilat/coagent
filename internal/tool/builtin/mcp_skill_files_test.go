package builtin

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/mcp"
)

func listedSkillFile(uri string, data []byte) mcp.SkillFile {
	return mcp.SkillFile{URI: uri, Size: int64(len(data)), Digest: fmt.Sprintf("sha256:%x", sha256.Sum256(data))}
}

func TestMCPSkillFiles_DocumentStoresAVerifiedCopy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	data := []byte("---\nname: pdf\ndescription: PDF\n---\nPDF instructions\n")
	reader := &skillReaderFake{data: data}
	dir, err := coagenthome.ProcessProjectDir(1)
	require.NoError(t, err)
	source := &mcpSkillFiles{
		reader:   reader,
		dir:      filepath.Join(dir, coagenthome.MCPSkillsDirName, "docs"),
		document: listedSkillFile("skill://pdf/SKILL.md", data),
	}
	got, err := source.Document(t.Context())
	require.NoError(t, err)
	assert.Equal(t, data, got)
	path := filepath.Join(source.dir, "skill", "pdf", "SKILL.md")
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o500), info.Mode().Perm())
	got, err = source.Document(t.Context())
	require.NoError(t, err)
	assert.Equal(t, data, got)
	assert.Equal(t, 1, reader.calls)
	require.NoError(t, os.Chmod(path, 0o600))
	require.NoError(t, os.WriteFile(path, []byte("changed"), 0o600))
	got, err = source.Document(t.Context())
	require.NoError(t, err)
	assert.Equal(t, data, got)
	assert.Equal(t, 2, reader.calls)
	info, err = os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o500), info.Mode().Perm())
	reader.err = errors.New("read failed")
	source.document = listedSkillFile("skill://pdf/SKILL.md", []byte("new version"))
	_, err = source.Document(t.Context())
	require.EqualError(t, err, "SKILL.md: read failed")
	files, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".fetch-*"))
	require.NoError(t, err)
	assert.Empty(t, files)
	t.Run("rename failure cleans temporary file", func(t *testing.T) {
		reader := &skillReaderFake{data: data}
		source := &mcpSkillFiles{
			reader:   reader,
			dir:      t.TempDir(),
			document: listedSkillFile("skill://pdf/SKILL.md", data),
		}
		path := filepath.Join(source.dir, "skill", "pdf", "SKILL.md")
		require.NoError(t, os.MkdirAll(path, 0o700))
		_, err := source.Document(t.Context())
		require.ErrorContains(t, err, "SKILL.md: replace skill file:")
		files, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".fetch-*"))
		require.NoError(t, err)
		assert.Empty(t, files)
	})
}

func TestMCPSkillPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, tc := range []struct{ uri, want string }{
		{uri: "skill://pdf/scripts/x.py", want: "skill/pdf/scripts/x.py"},
		{uri: "github://o/r/skills/refunds/SKILL.md", want: "github/o/r/skills/refunds/SKILL.md"},
		{uri: "skill://a/b%2F..%2Fc"},
		{uri: "skill://a/b%2Fc"},
		{uri: "skill://a/b/c", want: "skill/a/b/c"},
		{uri: "skill://a/%2E%2E/x"},
		{uri: "skill://a/my%20file.md", want: "skill/a/my file.md"},
		{uri: "skill://a//x"},
		{uri: "skill://user@a/x"},
		{uri: "skill://a:80/x"},
		{uri: "skill://a/x?q=y"},
		{uri: "skill://a/x#part"},
		{uri: "skill://a/%00"},
		{uri: "skill:///x"},
		{uri: "skill:opaque"},
		{uri: "skill://a/x?"},
	} {
		t.Run(tc.uri, func(t *testing.T) {
			got, err := mcpSkillPath(dir, tc.uri)
			if tc.want == "" {
				require.EqualError(t, err, "unsafe skill file URI "+tc.uri)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, filepath.Join(dir, tc.want), got)
		})
	}
}

func TestMCPSkillFiles_OpenRejectsUnlistedFile(t *testing.T) {
	t.Parallel()
	reader := &skillReaderFake{}
	source := &mcpSkillFiles{reader: reader}
	_, err := source.Open(t.Context(), "scripts/missing.sh")
	require.EqualError(t, err, "file scripts/missing.sh is not listed in the skill's manifest")
	_, err = source.Open(t.Context(), "SKILL.md")
	require.EqualError(t, err, "file SKILL.md is not listed in the skill's manifest")
	assert.Zero(t, reader.calls)
}
