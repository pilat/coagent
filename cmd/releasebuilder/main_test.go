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

	for _, input := range []string{
		"windows-amd64=coagent",
		"darwin-amd64=coagent",
		"darwin-arm64=coagent",
	} {
		_, err := buildArtifact(options{version: "v1.2.3", outDir: t.TempDir()}, input)
		require.Error(t, err, input)
	}
}

func TestValidateReleaseInputsRequiresExactlyBothLinuxTuples(t *testing.T) {
	t.Parallel()

	require.NoError(t, validateReleaseInputs([]string{
		"linux-amd64=a", "linux-arm64=b",
	}))

	for name, inputs := range map[string][]string{
		"darwin rejected":  {"linux-amd64=a", "darwin-amd64=b"},
		"missing arm64":    {"linux-amd64=a"},
		"missing amd64":    {"linux-arm64=b"},
		"empty":            {},
		"duplicate amd64":  {"linux-amd64=a", "linux-amd64=b", "linux-arm64=c"},
		"duplicate arm64":  {"linux-amd64=a", "linux-arm64=b", "linux-arm64=c"},
		"extra platform":   {"linux-amd64=a", "linux-arm64=b", "linux-386=c"},
		"malformed input":  {"linux-amd64=a", "linux-arm64"},
		"empty binary":     {"linux-amd64=a", "linux-arm64="},
		"windows rejected": {"linux-amd64=a", "windows-amd64=b"},
		"single darwin":    {"darwin-arm64=a"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			require.Error(t, validateReleaseInputs(inputs))
		})
	}
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
