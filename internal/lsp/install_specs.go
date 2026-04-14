package lsp

import "path/filepath"

const (
	linuxOS             = "linux"
	darwinOS            = "darwin"
	amd64Arch           = "amd64"
	arm64Arch           = "arm64"
	rustAnalyzerName    = "rust-analyzer"
	luaLSName           = "lua-language-server"
	luaLSArchiveEntry   = "bin/lua-language-server"
	terraformLSName     = "terraform-ls"
	rubyLSPName         = "ruby-lsp"
	goplsVersion        = "v0.23.0"
	typescriptLSVersion = "5.3.0"
	typescriptVersion   = "7.0.2"
	yamlLSVersion       = "1.24.0"
	pyrightVersion      = "1.1.413"
	jsonLSVersion       = "4.10.0"
	rubyLSPVersion      = "0.26.10"
	bashLSVersion       = "5.6.0"
	dockerLSVersion     = "0.15.0"
	intelephenseVersion = "1.18.5"
	rustAnalyzerVersion = "2026-07-27"
	luaLSVersion        = "3.13.5"
	terraformLSVersion  = "0.34.3"

	archiveGzip archiveKind = iota
	archiveTarGzip
	archiveZip
)

var releaseArtifacts = map[releaseKey]releaseArtifact{
	{name: rustAnalyzerName, goos: linuxOS, goarch: amd64Arch}: {
		url:    "https://github.com/rust-lang/rust-analyzer/releases/download/2026-07-27/rust-analyzer-x86_64-unknown-linux-gnu.gz",
		sha256: "ac4f42ddbbd040d75d847e991894776485783e28beb744b9719a660a99abe115",
		kind:   archiveGzip,
	},
	{name: rustAnalyzerName, goos: linuxOS, goarch: arm64Arch}: {
		url:    "https://github.com/rust-lang/rust-analyzer/releases/download/2026-07-27/rust-analyzer-aarch64-unknown-linux-gnu.gz",
		sha256: "4cb0ca4675608e8d73a7f4e43ef733d1f69600845d504c35d2f9d9f240bd3486",
		kind:   archiveGzip,
	},
	{name: rustAnalyzerName, goos: darwinOS, goarch: amd64Arch}: {
		url:    "https://github.com/rust-lang/rust-analyzer/releases/download/2026-07-27/rust-analyzer-x86_64-apple-darwin.gz",
		sha256: "9d1a60991ead6c27baa9d265fc8fd03bba9c39cf0ec2aaf389e37e6155af7cbb",
		kind:   archiveGzip,
	},
	{name: rustAnalyzerName, goos: darwinOS, goarch: arm64Arch}: {
		url:    "https://github.com/rust-lang/rust-analyzer/releases/download/2026-07-27/rust-analyzer-aarch64-apple-darwin.gz",
		sha256: "102215ae7e7a41c0dda8f24e910a01e757f58091204863e5e3e6696b743f7e97",
		kind:   archiveGzip,
	},
	{name: luaLSName, goos: linuxOS, goarch: amd64Arch}: {
		url:    "https://github.com/LuaLS/lua-language-server/releases/download/3.13.5/lua-language-server-3.13.5-linux-x64.tar.gz",
		sha256: "5d4316291b8c79b145002318fbb7cc294a327c314e2711e590609b178478eb59",
		kind:   archiveTarGzip,
		entry:  luaLSArchiveEntry,
	},
	{name: luaLSName, goos: linuxOS, goarch: arm64Arch}: {
		url:    "https://github.com/LuaLS/lua-language-server/releases/download/3.13.5/lua-language-server-3.13.5-linux-arm64.tar.gz",
		sha256: "ae3a05a1c6746e3ce9f29a9abd4e10b8a450c72ece53612343f4ac5fd11c6046",
		kind:   archiveTarGzip,
		entry:  luaLSArchiveEntry,
	},
	{name: luaLSName, goos: darwinOS, goarch: amd64Arch}: {
		url:    "https://github.com/LuaLS/lua-language-server/releases/download/3.13.5/lua-language-server-3.13.5-darwin-x64.tar.gz",
		sha256: "996b68ef058b951c4fabf89250e64d31deb2d285714c1aa2bb3cf78e07f9e332",
		kind:   archiveTarGzip,
		entry:  luaLSArchiveEntry,
	},
	{name: luaLSName, goos: darwinOS, goarch: arm64Arch}: {
		url:    "https://github.com/LuaLS/lua-language-server/releases/download/3.13.5/lua-language-server-3.13.5-darwin-arm64.tar.gz",
		sha256: "538c63153f4211e0b1e7131c6a68ab06fda450c7281756ac75675792d7a3a5b3",
		kind:   archiveTarGzip,
		entry:  luaLSArchiveEntry,
	},
	{name: terraformLSName, goos: linuxOS, goarch: amd64Arch}: {
		url:    "https://releases.hashicorp.com/terraform-ls/0.34.3/terraform-ls_0.34.3_linux_amd64.zip",
		sha256: "c36805a73af626a102985feddc4ac2d42a2c95580f353e9cb9601b66934cb32d",
		kind:   archiveZip,
		entry:  terraformLSName,
	},
	{name: terraformLSName, goos: linuxOS, goarch: arm64Arch}: {
		url:    "https://releases.hashicorp.com/terraform-ls/0.34.3/terraform-ls_0.34.3_linux_arm64.zip",
		sha256: "1bab3e09984763fe80b6e031b2f55aa6c41591485c4407764aedfcd52d0a04cd",
		kind:   archiveZip,
		entry:  terraformLSName,
	},
	{name: terraformLSName, goos: darwinOS, goarch: amd64Arch}: {
		url:    "https://releases.hashicorp.com/terraform-ls/0.34.3/terraform-ls_0.34.3_darwin_amd64.zip",
		sha256: "60e1aefe895acb87b62f62cc679e33726a4bb776ae5758e4c78c84a12b3a1245",
		kind:   archiveZip,
		entry:  terraformLSName,
	},
	{name: terraformLSName, goos: darwinOS, goarch: arm64Arch}: {
		url:    "https://releases.hashicorp.com/terraform-ls/0.34.3/terraform-ls_0.34.3_darwin_arm64.zip",
		sha256: "9d337d9c868379a717deb650363a1ccd84e309f4de9dbcd44d5f7809ebca66d0",
		kind:   archiveZip,
		entry:  terraformLSName,
	},
}

type archiveKind uint8

type npmInstallSpec struct {
	name        string
	rootVersion string
	packages    []string
	executable  string
}

type releaseArtifact struct {
	url    string
	sha256 string
	kind   archiveKind
	entry  string
}

type releaseKey struct {
	name   string
	goos   string
	goarch string
}

func releaseArtifactFor(name, goos, goarch string) (releaseArtifact, bool) {
	artifact, ok := releaseArtifacts[releaseKey{name: name, goos: goos, goarch: goarch}]

	return artifact, ok
}

func typescriptInstallSpec() npmInstallSpec {
	return npmInstallSpec{
		name:        "typescript-language-server",
		rootVersion: typescriptLSVersion + "-typescript-" + typescriptVersion,
		packages: []string{
			"typescript-language-server@" + typescriptLSVersion,
			"typescript@" + typescriptVersion,
		},
		executable: filepath.Join("node_modules", ".bin", "typescript-language-server"),
	}
}

func yamlInstallSpec() npmInstallSpec {
	return npmInstallSpec{
		name:        "yaml-language-server",
		rootVersion: yamlLSVersion,
		packages:    []string{"yaml-language-server@" + yamlLSVersion},
		executable:  filepath.Join("node_modules", ".bin", "yaml-language-server"),
	}
}

func pyrightInstallSpec() npmInstallSpec {
	return npmInstallSpec{
		name:        "pyright",
		rootVersion: pyrightVersion,
		packages:    []string{"pyright@" + pyrightVersion},
		executable:  filepath.Join("node_modules", ".bin", "pyright-langserver"),
	}
}

func jsonInstallSpec() npmInstallSpec {
	return npmInstallSpec{
		name:        "vscode-langservers-extracted",
		rootVersion: jsonLSVersion,
		packages:    []string{"vscode-langservers-extracted@" + jsonLSVersion},
		executable:  filepath.Join("node_modules", ".bin", "vscode-json-language-server"),
	}
}

func bashInstallSpec() npmInstallSpec {
	return npmInstallSpec{
		name:        "bash-language-server",
		rootVersion: bashLSVersion,
		packages:    []string{"bash-language-server@" + bashLSVersion},
		executable:  filepath.Join("node_modules", ".bin", "bash-language-server"),
	}
}

func dockerInstallSpec() npmInstallSpec {
	return npmInstallSpec{
		name:        "dockerfile-language-server-nodejs",
		rootVersion: dockerLSVersion,
		packages:    []string{"dockerfile-language-server-nodejs@" + dockerLSVersion},
		executable:  filepath.Join("node_modules", ".bin", "docker-langserver"),
	}
}

func intelephenseInstallSpec() npmInstallSpec {
	return npmInstallSpec{
		name:        "intelephense",
		rootVersion: intelephenseVersion,
		packages:    []string{"intelephense@" + intelephenseVersion},
		executable:  filepath.Join("node_modules", ".bin", "intelephense"),
	}
}

func goplsInstallArgs() []string {
	return []string{"install", "golang.org/x/tools/gopls@" + goplsVersion}
}

func npmInstallArgs(stage string, spec npmInstallSpec) []string {
	args := make([]string, 0, 6+len(spec.packages))
	args = append(args, "install", "--prefix", stage, "--no-audit", "--no-fund", "--package-lock=false")
	args = append(args, spec.packages...)

	return args
}

func rubyInstallArgs(stage string) []string {
	return []string{
		"install",
		"--install-dir", filepath.Join(stage, "gems"),
		"--bindir", filepath.Join(stage, "bin"),
		"--no-document",
		"--version", rubyLSPVersion,
		rubyLSPName,
	}
}
