package lsp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

const (
	rubyLauncher = `#!/usr/bin/env ruby
gem_home = File.expand_path("../gems", __dir__)
exec({"GEM_HOME" => gem_home, "GEM_PATH" => gem_home}, File.join(__dir__, "ruby-lsp-gem"), *ARGV)
`
)

// installLocks serializes publication across the LSP managers owned by separate sessions.
var installLocks sync.Map

func npmPackageInstall(ctx context.Context, base string, spec npmInstallSpec) (string, error) {
	finalRoot := filepath.Join(base, "lsp", spec.name, spec.rootVersion)
	target := filepath.Join(finalRoot, spec.executable)

	err := stageInstallDir(finalRoot, spec.executable, func(stage string) error {
		npm, err := exec.LookPath("npm")
		if err != nil {
			return fmt.Errorf("%s not found and npm not available", spec.name)
		}

		cmd, cancel := installCmd(ctx, npm, npmInstallArgs(stage, spec)...)
		defer cancel()

		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("install %s: %w\n%s", spec.name, err, output)
		}

		return nil
	})
	if err != nil {
		return "", err
	}

	return target, nil
}

func rubyPackageInstall(ctx context.Context, base string) (string, error) {
	finalRoot := filepath.Join(base, "lsp", rubyLSPName, rubyLSPVersion)
	target := filepath.Join(finalRoot, "bin", rubyLSPName)

	err := stageInstallDir(finalRoot, filepath.Join("bin", rubyLSPName), func(stage string) error {
		gem, err := exec.LookPath("gem")
		if err != nil {
			return errors.New("ruby-lsp not found and gem not available")
		}

		return populateRubyRoot(ctx, gem, stage)
	})
	if err != nil {
		return "", err
	}

	return target, nil
}

func populateRubyRoot(ctx context.Context, gem, stage string) error {
	binDir := filepath.Join(stage, "bin")

	cmd, cancel := installCmd(ctx, gem, rubyInstallArgs(stage)...)
	defer cancel()

	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("install ruby-lsp: %w\n%s", err, output)
	}

	generated := filepath.Join(binDir, rubyLSPName)
	if err := validateExecutable(generated); err != nil {
		return fmt.Errorf("validate generated ruby-lsp launcher: %w", err)
	}

	if err := os.Rename(generated, filepath.Join(binDir, "ruby-lsp-gem")); err != nil {
		return fmt.Errorf("preserve generated ruby-lsp launcher: %w", err)
	}

	return writeRubyLauncher(generated)
}

func writeRubyLauncher(path string) error {
	if err := os.WriteFile(path, []byte(rubyLauncher), 0o755); err != nil {
		return fmt.Errorf("write ruby-lsp launcher: %w", err)
	}

	return nil
}

func stageInstallDir(finalRoot, expectedRelative string, populate func(string) error) error {
	return withInstallLock(finalRoot, func() error {
		target := filepath.Join(finalRoot, expectedRelative)
		if err := validateExistingTarget(finalRoot, target); err != nil {
			return err
		}

		if executableExists(target) {
			return nil
		}

		return populateInstallDir(finalRoot, expectedRelative, populate)
	})
}

func populateInstallDir(finalRoot, expectedRelative string, populate func(string) error) error {
	parent := filepath.Dir(finalRoot)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create install parent %s: %w", parent, err)
	}

	stage, err := os.MkdirTemp(parent, ".lsp-install-*")
	if err != nil {
		return fmt.Errorf("create install staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(stage) }()

	if err := populate(stage); err != nil {
		return err
	}

	if err := validateContainedExecutable(stage, filepath.Join(stage, expectedRelative)); err != nil {
		return fmt.Errorf("validate staged executable: %w", err)
	}

	if err := os.Rename(stage, finalRoot); err != nil {
		target := filepath.Join(finalRoot, expectedRelative)
		if validateExistingTarget(finalRoot, target) == nil && executableExists(target) {
			return nil
		}

		return fmt.Errorf("publish install root %s: %w", finalRoot, err)
	}

	return nil
}

func stageInstallFile(destination, stagedName string, populate func(string) error) error {
	return withInstallLock(destination, func() error {
		if err := validateExistingTarget(destination, destination); err != nil {
			return err
		}

		if executableExists(destination) {
			return nil
		}

		return populateInstallFile(destination, stagedName, populate)
	})
}

func populateInstallFile(destination, stagedName string, populate func(string) error) error {
	dir := filepath.Dir(destination)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create install directory %s: %w", dir, err)
	}

	stage, err := os.MkdirTemp(dir, ".lsp-install-*")
	if err != nil {
		return fmt.Errorf("create install staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(stage) }()

	if err := populate(stage); err != nil {
		return err
	}

	staged := filepath.Join(stage, stagedName)
	if err := validateOwnedExecutable(staged); err != nil {
		return fmt.Errorf("validate staged executable: %w", err)
	}

	if err := os.Rename(staged, destination); err != nil {
		if validateExistingTarget(destination, destination) == nil && executableExists(destination) {
			return nil
		}

		return fmt.Errorf("publish executable %s: %w", destination, err)
	}

	return nil
}

func validateExistingTarget(ownerPath, target string) error {
	info, err := os.Lstat(ownerPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("inspect existing install %s: %w", ownerPath, err)
	}

	validationErr := validateOwnedExecutable(ownerPath)
	if ownerPath != target {
		if !info.IsDir() {
			validationErr = fmt.Errorf("%s is not a real directory", ownerPath)
		} else {
			validationErr = validateContainedExecutable(ownerPath, target)
		}
	}

	if validationErr != nil {
		return fmt.Errorf("existing install %s is invalid; remove it manually: %w", ownerPath, validationErr)
	}

	return nil
}

func validateExecutable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}

	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}

	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%s is not executable", path)
	}

	return nil
}

func validateOwnedExecutable(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("lstat %s: %w", path, err)
	}

	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a real regular file", path)
	}

	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%s is not executable", path)
	}

	return nil
}

func validateContainedExecutable(root, target string) error {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve install root %s: %w", root, err)
	}

	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return fmt.Errorf("resolve executable %s: %w", target, err)
	}

	relative, err := filepath.Rel(resolvedRoot, resolvedTarget)
	if err != nil {
		return fmt.Errorf("relate executable %s to %s: %w", target, root, err)
	}

	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("executable %s resolves outside install root %s", target, root)
	}

	return validateExecutable(target)
}

func executableExists(path string) bool {
	return validateExecutable(path) == nil
}

func withInstallLock(path string, fn func() error) error {
	actual, _ := installLocks.LoadOrStore(path, &sync.Mutex{})
	mu, _ := actual.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()

	return fn()
}
