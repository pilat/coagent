package lsp

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	languageIDCSharp = "csharp"
	stdioArg         = "--stdio"
)

type serverConfig struct {
	ID            string
	LanguageID    string
	LanguageIDFor func(path string) string
	Extensions    []string
	PathNames     []string
	Args          []string
	RootFinder    func(workDir, file string) (string, error)
	Spawn         func(ctx context.Context, root string) (*exec.Cmd, error)
}

func (s serverConfig) languageID(path string) string {
	if s.LanguageIDFor != nil {
		return s.LanguageIDFor(path)
	}

	return s.LanguageID
}

func (s serverConfig) matches(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	base := filepath.Base(path)

	for _, entry := range s.Extensions {
		isExtension := strings.HasPrefix(entry, ".")
		if isExtension && strings.EqualFold(ext, entry) {
			return true
		}

		if !isExtension && strings.EqualFold(base, entry) {
			return true
		}
	}

	return false
}

func defaultServers() []serverConfig {
	return []serverConfig{
		goPLS(),
		typeScript(),
		yamlLS(),
		rustAnalyzer(),
		pyright(),
		luaLS(),
		jsonLS(),
		cSharp(),
		cClangd(),
		rubyLSP(),
		bashLS(),
		dockerfileLS(),
		terraformLS(),
		phpIntelephense(),
	}
}

func goPLS() serverConfig {
	return serverConfig{
		ID:         "gopls",
		LanguageID: "go",
		Extensions: []string{".go"},
		PathNames:  []string{"gopls"},
		RootFinder: func(workDir, file string) (string, error) {
			// Look for go.work first
			if root := findNearestRoot(workDir, file, []string{"go.work"}); root != "" {
				return root, nil
			}
			// Fall back to go.mod
			return findNearestRoot(workDir, file, []string{"go.mod", "go.sum"}), nil
		},
	}
}

func typeScript() serverConfig {
	return serverConfig{
		ID:            "typescript-language-server",
		LanguageIDFor: languageIDTypeScript,
		Extensions:    []string{".ts", ".tsx", ".js", ".jsx", ".mjs"},
		PathNames:     []string{"typescript-language-server"},
		Args:          []string{stdioArg},
		RootFinder: func(workDir, file string) (string, error) {
			return findNearestRoot(workDir, file, []string{
				"package-lock.json", "bun.lockb", "bun.lock", "pnpm-lock.yaml", "yarn.lock",
			}), nil
		},
	}
}

func yamlLS() serverConfig {
	return serverConfig{
		ID:         "yaml-language-server",
		LanguageID: "yaml",
		Extensions: []string{".yaml", ".yml"},
		PathNames:  []string{"yaml-language-server"},
		Args:       []string{stdioArg},
		RootFinder: func(workDir, file string) (string, error) {
			return findNearestRoot(workDir, file, []string{
				"package-lock.json", "bun.lockb", "bun.lock", "pnpm-lock.yaml", "yarn.lock",
			}), nil
		},
	}
}

func rustAnalyzer() serverConfig {
	return serverConfig{
		ID:         "rust-analyzer",
		LanguageID: "rust",
		Extensions: []string{".rs"},
		PathNames:  []string{"rust-analyzer"},
		RootFinder: func(workDir, file string) (string, error) {
			return findNearestRoot(workDir, file, []string{"Cargo.toml"}), nil
		},
	}
}

func pyright() serverConfig {
	return serverConfig{
		ID:         "pyright",
		LanguageID: "python",
		Extensions: []string{".py", ".pyi"},
		PathNames:  []string{"pyright-langserver"},
		Args:       []string{stdioArg},
		RootFinder: func(workDir, file string) (string, error) {
			return findNearestRoot(workDir, file, []string{
				"pyproject.toml", "setup.py", "setup.cfg", "requirements.txt", "Pipfile",
			}), nil
		},
	}
}

func luaLS() serverConfig {
	return serverConfig{
		ID:         "lua-language-server",
		LanguageID: "lua",
		Extensions: []string{".lua"},
		PathNames:  []string{"lua-language-server"},
		RootFinder: func(workDir, file string) (string, error) {
			return workDir, nil
		},
	}
}

func jsonLS() serverConfig {
	return serverConfig{
		ID:         "vscode-json-language-server",
		LanguageID: "json",
		Extensions: []string{".json"},
		PathNames:  []string{"vscode-json-language-server"},
		Args:       []string{stdioArg},
		RootFinder: func(workDir, file string) (string, error) {
			return workDir, nil
		},
	}
}

func cSharp() serverConfig {
	return serverConfig{
		ID:         "csharp",
		LanguageID: languageIDCSharp,
		Extensions: []string{".cs"},
		PathNames:  []string{"omnisharp"},
		Args:       []string{"-lsp"},
		RootFinder: func(workDir, file string) (string, error) {
			return findNearestRoot(workDir, file, []string{"*.csproj", "*.sln"}), nil
		},
	}
}

func cClangd() serverConfig {
	return serverConfig{
		ID:            "clangd",
		LanguageIDFor: languageIDC,
		Extensions:    []string{".c", ".cpp", ".h", ".hpp", ".cc", ".cxx"},
		PathNames:     []string{"clangd"},
		RootFinder: func(workDir, file string) (string, error) {
			return findNearestRoot(workDir, file, []string{"compile_commands.json", ".clangd", "CMakeLists.txt"}), nil
		},
	}
}

func languageIDTypeScript(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".ts":
		return "typescript"
	case ".tsx":
		return "typescriptreact"
	case ".jsx":
		return "javascriptreact"
	default:
		return "javascript"
	}
}

func languageIDC(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".c", ".h":
		return "c"
	default:
		return "cpp"
	}
}

func rubyLSP() serverConfig {
	return serverConfig{
		ID:         "ruby-lsp",
		LanguageID: "ruby",
		Extensions: []string{".rb", ".rake", ".gemspec", ".ru"},
		PathNames:  []string{"ruby-lsp"},
		RootFinder: func(workDir, file string) (string, error) {
			return findNearestRoot(workDir, file, []string{"Gemfile"}), nil
		},
	}
}

func bashLS() serverConfig {
	return serverConfig{
		ID:         "bash-language-server",
		LanguageID: "shellscript",
		Extensions: []string{".sh", ".bash"},
		PathNames:  []string{"bash-language-server"},
		Args:       []string{"start"},
		RootFinder: func(workDir, file string) (string, error) {
			return workDir, nil
		},
	}
}

func dockerfileLS() serverConfig {
	return serverConfig{
		ID:         "dockerfile-ls",
		LanguageID: "dockerfile",
		Extensions: []string{"Dockerfile", ".dockerfile"},
		PathNames:  []string{"docker-langserver"},
		Args:       []string{stdioArg},
		RootFinder: func(workDir, file string) (string, error) {
			return workDir, nil
		},
	}
}

func terraformLS() serverConfig {
	return serverConfig{
		ID:         "terraform-ls",
		LanguageID: "terraform",
		Extensions: []string{".tf", ".tfvars"},
		PathNames:  []string{"terraform-ls"},
		Args:       []string{"serve"},
		RootFinder: func(workDir, file string) (string, error) {
			return findNearestRootMarkers(workDir, file, []rootMarker{exactDir(".terraform"), filePattern("*.tf")}), nil
		},
	}
}

func phpIntelephense() serverConfig {
	return serverConfig{
		ID:         "php-intelephense",
		LanguageID: "php",
		Extensions: []string{".php"},
		PathNames:  []string{"intelephense"},
		Args:       []string{stdioArg},
		RootFinder: func(workDir, file string) (string, error) {
			return findNearestRoot(workDir, file, []string{"composer.json", "composer.lock"}), nil
		},
	}
}
