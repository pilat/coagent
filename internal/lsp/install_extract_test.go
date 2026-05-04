package lsp

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var extractArtifactCases = []struct {
	name     string
	artifact releaseArtifact
	archive  func(*testing.T) []byte
	want     string
	wantErr  string
}{
	{
		name:     "gzip executable",
		artifact: releaseArtifact{kind: archiveGzip},
		archive:  func(t *testing.T) []byte { return gzipTestBytes(t, []byte("raw")) },
		want:     "raw",
	},
	{
		name:     "tar gzip exact entry",
		artifact: releaseArtifact{kind: archiveTarGzip, entry: "bin/server"},
		archive: func(t *testing.T) []byte {
			return tarGzipTestBytes(t, []testArchiveEntry{{name: "bin/server", content: "tar"}})
		},
		want: "tar",
	},
	{
		name:     "zip exact entry",
		artifact: releaseArtifact{kind: archiveZip, entry: "server"},
		archive: func(t *testing.T) []byte {
			return zipTestBytes(t, []testArchiveEntry{{name: "server", content: "zip"}})
		},
		want: "zip",
	},
	{
		name:     "malformed gzip",
		artifact: releaseArtifact{kind: archiveGzip},
		archive:  func(*testing.T) []byte { return []byte("bad") },
		wantErr:  "gzip",
	},
	{
		name:     "tar missing entry",
		artifact: releaseArtifact{kind: archiveTarGzip, entry: "bin/server"},
		archive: func(t *testing.T) []byte {
			return tarGzipTestBytes(t, []testArchiveEntry{{name: "README", content: "text"}})
		},
		wantErr: "not found",
	},
	{
		name:     "tar target at unexpected path",
		artifact: releaseArtifact{kind: archiveTarGzip, entry: "bin/server"},
		archive: func(t *testing.T) []byte {
			return tarGzipTestBytes(t, []testArchiveEntry{{name: "../bin/server", content: "bad"}})
		},
		wantErr: "unexpected path",
	},
	{
		name:     "tar duplicate entry",
		artifact: releaseArtifact{kind: archiveTarGzip, entry: "bin/server"},
		archive: func(t *testing.T) []byte {
			return tarGzipTestBytes(t, []testArchiveEntry{
				{name: "bin/server", content: "one"},
				{name: "bin/server", content: "two"},
			})
		},
		wantErr: "duplicate",
	},
	{
		name:     "tar symlink entry",
		artifact: releaseArtifact{kind: archiveTarGzip, entry: "bin/server"},
		archive: func(t *testing.T) []byte {
			return tarGzipTestBytes(t, []testArchiveEntry{{name: "bin/server", typeFlag: tar.TypeSymlink}})
		},
		wantErr: "not a regular file",
	},
	{
		name:     "zip missing entry",
		artifact: releaseArtifact{kind: archiveZip, entry: "server"},
		archive: func(t *testing.T) []byte {
			return zipTestBytes(t, []testArchiveEntry{{name: "README", content: "text"}})
		},
		wantErr: "not found",
	},
	{
		name:     "zip target at unexpected path",
		artifact: releaseArtifact{kind: archiveZip, entry: "server"},
		archive: func(t *testing.T) []byte {
			return zipTestBytes(t, []testArchiveEntry{{name: "bin/server", content: "bad"}})
		},
		wantErr: "unexpected path",
	},
	{
		name:     "zip duplicate entry",
		artifact: releaseArtifact{kind: archiveZip, entry: "server"},
		archive: func(t *testing.T) []byte {
			return zipTestBytes(t, []testArchiveEntry{
				{name: "server", content: "one"},
				{name: "server", content: "two"},
			})
		},
		wantErr: "duplicate",
	},
	{
		name:     "zip symlink entry",
		artifact: releaseArtifact{kind: archiveZip, entry: "server"},
		archive: func(t *testing.T) []byte {
			return zipTestBytes(t, []testArchiveEntry{{
				name: "server", content: "target", mode: os.ModeSymlink | 0o777,
			}})
		},
		wantErr: "not a regular file",
	},
}

type testArchiveEntry struct {
	name     string
	content  string
	typeFlag byte
	mode     os.FileMode
}

func TestExtractArtifact(t *testing.T) {
	for _, tt := range extractArtifactCases {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			archivePath := filepath.Join(dir, "archive")
			destination := filepath.Join(dir, "server")
			require.NoError(t, os.WriteFile(archivePath, tt.archive(t), 0o600))

			err := extractArtifact(archivePath, destination, tt.artifact)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				assert.NoFileExists(t, destination)

				return
			}

			require.NoError(t, err)
			content, err := os.ReadFile(destination)
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(content))
			info, err := os.Stat(destination)
			require.NoError(t, err)
			assert.NotZero(t, info.Mode().Perm()&0o111)
		})
	}
}

func TestExtractArtifactPreservesExistingDestination(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "archive")
	destination := filepath.Join(dir, "server")
	require.NoError(t, os.WriteFile(archivePath, gzipTestBytes(t, []byte("new")), 0o600))
	require.NoError(t, os.WriteFile(destination, []byte("old"), 0o755))

	err := extractArtifact(archivePath, destination, releaseArtifact{kind: archiveGzip})
	require.Error(t, err)
	content, readErr := os.ReadFile(destination)
	require.NoError(t, readErr)
	assert.Equal(t, "old", string(content))
}

func TestCopyExecutableRejectsOversizePayload(t *testing.T) {
	originalLimit := maxLSPExecutableBytes
	maxLSPExecutableBytes = 4
	defer func() { maxLSPExecutableBytes = originalLimit }()

	var dst bytes.Buffer
	err := copyExecutable(&dst, bytes.NewBufferString("12345"), "test payload")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds 4 bytes")
}

func tarGzipTestBytes(t *testing.T, entries []testArchiveEntry) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, entry := range entries {
		typeFlag := entry.typeFlag
		if typeFlag == 0 {
			typeFlag = tar.TypeReg
		}
		header := &tar.Header{
			Name:     entry.name,
			Mode:     0o755,
			Size:     int64(len(entry.content)),
			Typeflag: typeFlag,
		}
		if typeFlag != tar.TypeReg {
			header.Size = 0
		}
		require.NoError(t, tw.WriteHeader(header))
		if header.Size > 0 {
			_, err := tw.Write([]byte(entry.content))
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	require.NoError(t, zw.Close())

	return buf.Bytes()
}

func zipTestBytes(t *testing.T, entries []testArchiveEntry) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		mode := entry.mode
		if mode == 0 {
			mode = 0o755
		}
		header.SetMode(mode)
		writer, err := zw.CreateHeader(header)
		require.NoError(t, err)
		_, err = writer.Write([]byte(entry.content))
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())

	return buf.Bytes()
}
