package lsp

import (
	"context"
	"os/exec"
)

type serverConfig struct {
	ID         string
	Extensions []string
	RootFinder func(workDir, file string) (string, error)
	Spawn      func(ctx context.Context, root string) (*exec.Cmd, error)
}

func defaultServers(coagentBin string) []serverConfig {
	return []serverConfig{
		goPLS(coagentBin),
		typeScript(coagentBin),
		yamlLS(coagentBin),
		rustAnalyzer(coagentBin),
		pyright(coagentBin),
		luaLS(coagentBin),
		jsonLS(coagentBin),
		cSharp(),
		cClangd(),
		rubyLSP(coagentBin),
		bashLS(coagentBin),
		dockerfileLS(coagentBin),
		terraformLS(coagentBin),
		phpIntelephense(coagentBin),
	}
}

func goPLS(coagentBin string) serverConfig {
	return serverConfig{
		ID:         "gopls",
		Extensions: []string{".go"},
		RootFinder: func(workDir, file string) (string, error) {
			// Look for go.work first
			if root := findNearestRoot(workDir, file, []string{"go.work"}); root != "" {
				return root, nil
			}
			// Fall back to go.mod
			return findNearestRoot(workDir, file, []string{"go.mod", "go.sum"}), nil
		},
		Spawn: func(ctx context.Context, root string) (*exec.Cmd, error) {
			bin, err := findOrInstallGopls(ctx, coagentBin)
			if err != nil {
				return nil, err
			}

			return exec.CommandContext(ctx, bin), nil
		},
	}
}

func typeScript(coagentBin string) serverConfig {
	return serverConfig{
		ID:         "typescript",
		Extensions: []string{".ts", ".tsx", ".js", ".jsx", ".mjs"},
		RootFinder: func(workDir, file string) (string, error) {
			return findNearestRoot(workDir, file, []string{
				"package-lock.json", "bun.lockb", "bun.lock", "pnpm-lock.yaml", "yarn.lock",
			}), nil
		},
		Spawn: func(ctx context.Context, root string) (*exec.Cmd, error) {
			bin, err := findOrInstallTypescript(ctx, coagentBin)
			if err != nil {
				return nil, err
			}

			return exec.CommandContext(ctx, bin, "--stdio"), nil
		},
	}
}

func yamlLS(coagentBin string) serverConfig {
	return serverConfig{
		ID:         "yaml-ls",
		Extensions: []string{".yaml", ".yml"},
		RootFinder: func(workDir, file string) (string, error) {
			return findNearestRoot(workDir, file, []string{
				"package-lock.json", "bun.lockb", "bun.lock", "pnpm-lock.yaml", "yarn.lock",
			}), nil
		},
		Spawn: func(ctx context.Context, root string) (*exec.Cmd, error) {
			bin, args, err := findOrInstallYamlLS(ctx, coagentBin)
			if err != nil {
				return nil, err
			}

			return exec.CommandContext(ctx, bin, args...), nil
		},
	}
}

func rustAnalyzer(coagentBin string) serverConfig {
	return serverConfig{
		ID:         rustAnalyzerName,
		Extensions: []string{".rs"},
		RootFinder: func(workDir, file string) (string, error) {
			return findNearestRoot(workDir, file, []string{"Cargo.toml"}), nil
		},
		Spawn: func(ctx context.Context, root string) (*exec.Cmd, error) {
			bin, err := findOrInstallRustAnalyzer(ctx, coagentBin)
			if err != nil {
				return nil, err
			}

			return exec.CommandContext(ctx, bin), nil
		},
	}
}

func pyright(coagentBin string) serverConfig {
	return serverConfig{
		ID:         "pyright",
		Extensions: []string{".py", ".pyi"},
		RootFinder: func(workDir, file string) (string, error) {
			return findNearestRoot(workDir, file, []string{
				"pyproject.toml", "setup.py", "setup.cfg", "requirements.txt", "Pipfile",
			}), nil
		},
		Spawn: func(ctx context.Context, root string) (*exec.Cmd, error) {
			bin, err := findOrInstallPyright(ctx, coagentBin)
			if err != nil {
				return nil, err
			}

			return exec.CommandContext(ctx, bin, "--stdio"), nil
		},
	}
}

func luaLS(coagentBin string) serverConfig {
	return serverConfig{
		ID:         "lua-ls",
		Extensions: []string{".lua"},
		RootFinder: func(workDir, file string) (string, error) {
			return workDir, nil
		},
		Spawn: func(ctx context.Context, root string) (*exec.Cmd, error) {
			bin, err := findOrInstallLuaLS(ctx, coagentBin)
			if err != nil {
				return nil, err
			}

			return exec.CommandContext(ctx, bin), nil
		},
	}
}

func jsonLS(coagentBin string) serverConfig {
	return serverConfig{
		ID:         "json-ls",
		Extensions: []string{".json"},
		RootFinder: func(workDir, file string) (string, error) {
			return workDir, nil
		},
		Spawn: func(ctx context.Context, root string) (*exec.Cmd, error) {
			bin, err := findOrInstallJSONLS(ctx, coagentBin)
			if err != nil {
				return nil, err
			}

			return exec.CommandContext(ctx, bin, "--stdio"), nil
		},
	}
}

func cSharp() serverConfig {
	return serverConfig{
		ID:         "csharp",
		Extensions: []string{".cs"},
		RootFinder: func(workDir, file string) (string, error) {
			return findNearestRoot(workDir, file, []string{"*.csproj", "*.sln"}), nil
		},
		Spawn: func(ctx context.Context, root string) (*exec.Cmd, error) {
			bin := "omnisharp"
			if b, err := exec.LookPath(bin); err == nil {
				bin = b
			}

			return exec.CommandContext(ctx, bin, "-lsp"), nil
		},
	}
}

func cClangd() serverConfig {
	return serverConfig{
		ID:         "clangd",
		Extensions: []string{".c", ".cpp", ".h", ".hpp", ".cc", ".cxx"},
		RootFinder: func(workDir, file string) (string, error) {
			return findNearestRoot(workDir, file, []string{"compile_commands.json", ".clangd", "CMakeLists.txt"}), nil
		},
		Spawn: func(ctx context.Context, root string) (*exec.Cmd, error) {
			bin, err := findOrInstallClangd()
			if err != nil {
				return nil, err
			}

			return exec.CommandContext(ctx, bin), nil
		},
	}
}

func rubyLSP(coagentBin string) serverConfig {
	return serverConfig{
		ID:         rubyLSPName,
		Extensions: []string{".rb", ".rake", ".gemspec", ".ru"},
		RootFinder: func(workDir, file string) (string, error) {
			return findNearestRoot(workDir, file, []string{"Gemfile"}), nil
		},
		Spawn: func(ctx context.Context, root string) (*exec.Cmd, error) {
			bin, err := findOrInstallRubyLSP(ctx, coagentBin)
			if err != nil {
				return nil, err
			}

			return exec.CommandContext(ctx, bin), nil
		},
	}
}

func bashLS(coagentBin string) serverConfig {
	return serverConfig{
		ID:         "bash-ls",
		Extensions: []string{".sh", ".bash"},
		RootFinder: func(workDir, file string) (string, error) {
			return workDir, nil
		},
		Spawn: func(ctx context.Context, root string) (*exec.Cmd, error) {
			bin, err := findOrInstallBashLS(ctx, coagentBin)
			if err != nil {
				return nil, err
			}

			return exec.CommandContext(ctx, bin, "start"), nil
		},
	}
}

func dockerfileLS(coagentBin string) serverConfig {
	return serverConfig{
		ID:         "dockerfile-ls",
		Extensions: []string{"Dockerfile", ".dockerfile"},
		RootFinder: func(workDir, file string) (string, error) {
			return workDir, nil
		},
		Spawn: func(ctx context.Context, root string) (*exec.Cmd, error) {
			bin, err := findOrInstallDockerfileLS(ctx, coagentBin)
			if err != nil {
				return nil, err
			}

			return exec.CommandContext(ctx, bin, "--stdio"), nil
		},
	}
}

func terraformLS(coagentBin string) serverConfig {
	return serverConfig{
		ID:         terraformLSName,
		Extensions: []string{".tf", ".tfvars"},
		RootFinder: func(workDir, file string) (string, error) {
			return findNearestRoot(workDir, file, []string{".terraform", "*.tf"}), nil
		},
		Spawn: func(ctx context.Context, root string) (*exec.Cmd, error) {
			bin, err := findOrInstallTerraformLS(ctx, coagentBin)
			if err != nil {
				return nil, err
			}

			return exec.CommandContext(ctx, bin, "serve"), nil
		},
	}
}

func phpIntelephense(coagentBin string) serverConfig {
	return serverConfig{
		ID:         "php-intelephense",
		Extensions: []string{".php"},
		RootFinder: func(workDir, file string) (string, error) {
			return findNearestRoot(workDir, file, []string{"composer.json", "composer.lock"}), nil
		},
		Spawn: func(ctx context.Context, root string) (*exec.Cmd, error) {
			bin, args, err := findOrInstallIntelephense(ctx, coagentBin)
			if err != nil {
				return nil, err
			}

			return exec.CommandContext(ctx, bin, args...), nil
		},
	}
}
