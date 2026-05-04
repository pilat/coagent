package lsp

import (
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var expectedNPMInstallSpecs = []npmInstallSpec{
	{
		name: "typescript-language-server", rootVersion: "5.3.0-typescript-7.0.2",
		packages:   []string{"typescript-language-server@5.3.0", "typescript@7.0.2"},
		executable: filepath.Join("node_modules", ".bin", "typescript-language-server"),
	},
	{
		name: "yaml-language-server", rootVersion: "1.24.0",
		packages:   []string{"yaml-language-server@1.24.0"},
		executable: filepath.Join("node_modules", ".bin", "yaml-language-server"),
	},
	{
		name: "pyright", rootVersion: "1.1.413",
		packages:   []string{"pyright@1.1.413"},
		executable: filepath.Join("node_modules", ".bin", "pyright-langserver"),
	},
	{
		name: "vscode-langservers-extracted", rootVersion: "4.10.0",
		packages:   []string{"vscode-langservers-extracted@4.10.0"},
		executable: filepath.Join("node_modules", ".bin", "vscode-json-language-server"),
	},
	{
		name: "bash-language-server", rootVersion: "5.6.0",
		packages:   []string{"bash-language-server@5.6.0"},
		executable: filepath.Join("node_modules", ".bin", "bash-language-server"),
	},
	{
		name: "dockerfile-language-server-nodejs", rootVersion: "0.15.0",
		packages:   []string{"dockerfile-language-server-nodejs@0.15.0"},
		executable: filepath.Join("node_modules", ".bin", "docker-langserver"),
	},
	{
		name: "intelephense", rootVersion: "1.18.5",
		packages:   []string{"intelephense@1.18.5"},
		executable: filepath.Join("node_modules", ".bin", "intelephense"),
	},
}

func TestPinnedInstallVersions(t *testing.T) {
	versions := []string{
		goplsVersion,
		typescriptLSVersion,
		typescriptVersion,
		yamlLSVersion,
		pyrightVersion,
		jsonLSVersion,
		rubyLSPVersion,
		bashLSVersion,
		dockerLSVersion,
		intelephenseVersion,
		rustAnalyzerVersion,
		luaLSVersion,
		terraformLSVersion,
	}

	for _, version := range versions {
		assert.NotEmpty(t, version)
		assert.NotEqual(t, "latest", version)
	}
}

func TestNPMInstallSpecsUseExactPackagesAndVersionedRoots(t *testing.T) {
	actual := []npmInstallSpec{
		typescriptInstallSpec(),
		yamlInstallSpec(),
		pyrightInstallSpec(),
		jsonInstallSpec(),
		bashInstallSpec(),
		dockerInstallSpec(),
		intelephenseInstallSpec(),
	}

	assert.Equal(t, expectedNPMInstallSpecs, actual)
}

func TestPackageManagerInstallArgumentsAreExact(t *testing.T) {
	assert.Equal(t, []string{
		"install", "golang.org/x/tools/gopls@v0.23.0",
	}, goplsInstallArgs())
	assert.Equal(t, []string{
		"install", "--prefix", "/stage", "--no-audit", "--no-fund", "--package-lock=false",
		"typescript-language-server@5.3.0", "typescript@7.0.2",
	}, npmInstallArgs("/stage", typescriptInstallSpec()))
	assert.Equal(t, []string{
		"install",
		"--install-dir", filepath.Join("/stage", "gems"),
		"--bindir", filepath.Join("/stage", "bin"),
		"--no-document",
		"--version", "0.26.10",
		"ruby-lsp",
	}, rubyInstallArgs("/stage"))
}

func TestReleaseArtifactMatrix(t *testing.T) {
	servers := []struct {
		name    string
		version string
	}{
		{name: "rust-analyzer", version: rustAnalyzerVersion},
		{name: "lua-language-server", version: luaLSVersion},
		{name: "terraform-ls", version: terraformLSVersion},
	}
	platforms := []struct {
		goos   string
		goarch string
	}{
		{goos: "linux", goarch: "amd64"},
		{goos: "linux", goarch: "arm64"},
		{goos: "darwin", goarch: "amd64"},
		{goos: "darwin", goarch: "arm64"},
	}

	for _, server := range servers {
		for _, platform := range platforms {
			name := server.name + "/" + platform.goos + "/" + platform.goarch
			t.Run(name, func(t *testing.T) {
				artifact, ok := releaseArtifactFor(server.name, platform.goos, platform.goarch)
				require.True(t, ok)
				assert.Contains(t, artifact.url, server.version)
				assert.NotContains(t, artifact.url, "/latest/")
				assert.True(t, strings.HasPrefix(artifact.url, "https://"))
				digest, err := hex.DecodeString(artifact.sha256)
				require.NoError(t, err)
				assert.Len(t, digest, 32)
			})
		}
	}

	assert.Len(t, releaseArtifacts, len(servers)*len(platforms))
}

func TestReleaseArtifactMatrixRejectsUnsupportedPlatform(t *testing.T) {
	_, ok := releaseArtifactFor("rust-analyzer", "windows", "amd64")
	assert.False(t, ok)
	_, ok = releaseArtifactFor("rust-analyzer", "linux", "386")
	assert.False(t, ok)
}
