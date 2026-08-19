package main

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteArchiveIsDeterministicAndMinimal(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	binary := filepath.Join(dir, "binary")
	license := filepath.Join(dir, "license")
	require.NoError(t, os.WriteFile(binary, []byte("binary"), 0o755))
	require.NoError(t, os.WriteFile(license, []byte("license"), 0o644))
	epoch := time.Unix(1_700_000_000, 0).UTC()

	first := filepath.Join(dir, "first.tar.gz")
	second := filepath.Join(dir, "second.tar.gz")
	require.NoError(t, writeArchive(first, binary, license, epoch))
	require.NoError(t, writeArchive(second, binary, license, epoch))

	firstData, err := os.ReadFile(first)
	require.NoError(t, err)
	secondData, err := os.ReadFile(second)
	require.NoError(t, err)
	assert.Equal(t, firstData, secondData)
	assert.Equal(t, []string{"coagent", "LICENSE"}, archiveNames(t, first))
}

func TestParseOptionsRejectsUnsafeVersion(t *testing.T) {
	t.Parallel()

	_, err := parseOptions([]string{
		"-version", "v1.2.3/../../escape", "-epoch", "1700000000", "linux-amd64=coagent",
	})
	require.Error(t, err)
}

func TestBuildArtifactRejectsUnsupportedPlatform(t *testing.T) {
	t.Parallel()

	_, err := buildArtifact(options{version: "v1.2.3", outDir: t.TempDir()}, "windows-amd64=coagent")
	require.Error(t, err)
}

func archiveNames(t *testing.T, path string) []string {
	t.Helper()
	file, err := os.Open(path)
	require.NoError(t, err)
	defer file.Close()
	gz, err := gzip.NewReader(file)
	require.NoError(t, err)
	defer gz.Close()

	var names []string
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		names = append(names, header.Name)
	}
	return names
}
