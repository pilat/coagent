package lsp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/pilat/coagent/internal/coagenthome"
)

const (
	flagStdio = "--stdio"
)

var lspInstallTimeout = 2 * time.Minute

func coagentBin() string {
	dir, err := coagenthome.Join(coagenthome.BinDirName)
	if err != nil {
		return ""
	}

	return dir
}

func ensureCoagentBin() error {
	bin := coagentBin()
	if bin == "" {
		return errors.New("coagent bin dir: home directory unresolvable")
	}

	return ensureInstallDir(bin)
}

func installCmd(ctx context.Context, name string, args ...string) (*exec.Cmd, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(ctx, lspInstallTimeout)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 10 * time.Second

	return cmd, cancel
}

func findBinary(pathNames, coagentPaths []string) (string, bool) {
	for _, name := range pathNames {
		if bin, err := exec.LookPath(name); err == nil {
			return bin, true
		}
	}

	for _, candidate := range coagentPaths {
		if executableExists(candidate) {
			return candidate, true
		}
	}

	return "", false
}

func findOrInstallGopls(ctx context.Context, base string) (string, error) {
	bin := filepath.Join(base, "gopls")
	if found, ok := findBinary([]string{"gopls"}, []string{bin}); ok {
		return found, nil
	}

	err := stageInstallFile(bin, "gopls", func(stage string) error {
		goBin, err := exec.LookPath("go")
		if err != nil {
			return errors.New("gopls not found and go not available")
		}

		cmd, cancel := installCmd(ctx, goBin, goplsInstallArgs()...)
		defer cancel()

		cmd.Env = append(os.Environ(), "GOBIN="+stage)
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("install gopls: %w\n%s", err, output)
		}

		return nil
	})
	if err != nil {
		return "", err
	}

	return bin, nil
}

func findOrInstallTypescript(ctx context.Context, base string) (string, error) {
	return findOrInstallNPM(ctx, base, []string{"typescript-language-server"}, typescriptInstallSpec())
}

func findOrInstallYamlLS(ctx context.Context, base string) (string, []string, error) {
	if bin, err := exec.LookPath("yaml-language-server"); err == nil {
		return bin, []string{flagStdio}, nil
	}

	legacy := filepath.Join(base, "node_modules", "yaml-language-server", "bin", "yaml-language-server")
	if regularFileExists(legacy) {
		return "node", []string{legacy, flagStdio}, nil
	}

	bin, err := npmPackageInstall(ctx, base, yamlInstallSpec())
	if err != nil {
		return "", nil, err
	}

	return bin, []string{flagStdio}, nil
}

func findOrInstallRustAnalyzer(ctx context.Context, base string) (string, error) {
	return findOrInstallRelease(ctx, base, rustAnalyzerName, rustAnalyzerName)
}

func findOrInstallPyright(ctx context.Context, base string) (string, error) {
	return findOrInstallNPM(ctx, base, []string{"pyright-langserver"}, pyrightInstallSpec())
}

func findOrInstallLuaLS(ctx context.Context, base string) (string, error) {
	return findOrInstallRelease(ctx, base, luaLSName, luaLSName)
}

func findOrInstallJSONLS(ctx context.Context, base string) (string, error) {
	return findOrInstallNPM(ctx, base, []string{"vscode-json-language-server"}, jsonInstallSpec())
}

func findOrInstallClangd() (string, error) {
	if bin, err := exec.LookPath("clangd"); err == nil {
		return bin, nil
	}

	return "", errors.New("clangd not found in PATH. Please install LLVM/clangd")
}

func findOrInstallRubyLSP(ctx context.Context, base string) (string, error) {
	legacy := filepath.Join(base, rubyLSPName)
	if found, ok := findBinary([]string{rubyLSPName}, []string{legacy}); ok {
		return found, nil
	}

	if err := ensureInstallDir(base); err != nil {
		return "", err
	}

	return rubyPackageInstall(ctx, base)
}

func findOrInstallBashLS(ctx context.Context, base string) (string, error) {
	return findOrInstallNPM(ctx, base, []string{"bash-language-server"}, bashInstallSpec())
}

func findOrInstallDockerfileLS(ctx context.Context, base string) (string, error) {
	return findOrInstallNPM(ctx, base, []string{"docker-langserver"}, dockerInstallSpec())
}

func findOrInstallTerraformLS(ctx context.Context, base string) (string, error) {
	return findOrInstallRelease(ctx, base, terraformLSName, terraformLSName)
}

func findOrInstallIntelephense(ctx context.Context, base string) (string, []string, error) {
	if bin, err := exec.LookPath("intelephense"); err == nil {
		return bin, []string{flagStdio}, nil
	}

	legacy := filepath.Join(base, "node_modules", "intelephense", "lib", "intelephense.js")
	if regularFileExists(legacy) {
		return "node", []string{legacy, flagStdio}, nil
	}

	bin, err := npmPackageInstall(ctx, base, intelephenseInstallSpec())
	if err != nil {
		return "", nil, err
	}

	return bin, []string{flagStdio}, nil
}

func findOrInstallNPM(
	ctx context.Context,
	base string,
	pathNames []string,
	spec npmInstallSpec,
) (string, error) {
	legacy := filepath.Join(base, "bin", filepath.Base(spec.executable))
	if found, ok := findBinary(pathNames, []string{legacy}); ok {
		return found, nil
	}

	if err := ensureInstallDir(base); err != nil {
		return "", err
	}

	return npmPackageInstall(ctx, base, spec)
}

func findOrInstallRelease(ctx context.Context, base, binaryName, releaseName string) (string, error) {
	bin := filepath.Join(base, binaryName)
	if found, ok := findBinary([]string{binaryName}, []string{bin}); ok {
		return found, nil
	}

	if err := ensureInstallDir(base); err != nil {
		return "", err
	}

	client := &http.Client{Timeout: lspInstallTimeout}
	if err := installRelease(ctx, client, bin, releaseName, runtime.GOOS, runtime.GOARCH); err != nil {
		return "", err
	}

	return bin, nil
}

func ensureInstallDir(dir string) error {
	if dir == "" {
		return errors.New("coagent bin dir: home directory unresolvable")
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create coagent bin dir: %w", err)
	}

	return nil
}

func regularFileExists(path string) bool {
	info, err := os.Stat(path)

	return err == nil && info.Mode().IsRegular()
}
