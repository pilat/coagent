package builtin

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/safefile"
)

// rejectNonRegular errors if path exists and is not a regular file. os.Open /
// os.ReadFile / os.WriteFile on a FIFO/device/socket blocks in the kernel,
// uncancelable by ctx; the stat gate catches it (stat never blocks). A missing
// path is allowed — a writer creates a fresh regular file.
func rejectNonRegular(path string) error {
	info, err := os.Stat(path)
	if err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular file: %s", path)
	}

	return nil
}

//nolint:wsl_v5 // Missing and non-regular targets are the two rooted metadata outcomes.
func rejectNonRegularAccess(access safefile.Access, path string) error {
	if access == nil || access.Scope() != safefile.ProjectConfined {
		return rejectNonRegular(path)
	}

	info, _, err := access.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect rooted mutation target: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refusing to mutate non-regular file: %s", path)
	}

	return nil
}

//nolint:wsl_v5 // Rooted and host-readable target resolution converge here.
func resolveAccessTarget(access safefile.Access, workDir, name string) (string, error) {
	if access == nil {
		return resolvePath(workDir, name), nil
	}
	path, err := access.ResolveTarget(name)
	if err != nil {
		return "", fmt.Errorf("authorize filesystem target: %w", err)
	}

	return canonicalExistingPath(path.Canonical), nil
}

//nolint:wsl_v5 // The rooted handle remains open through validation and read.
func readAccessFile(access safefile.Access, name string) ([]byte, error) {
	if access == nil {
		content, err := os.ReadFile(name)
		if err != nil {
			return nil, fmt.Errorf("read file: %w", err)
		}

		return content, nil
	}
	opened, err := access.Open(name)
	if err != nil {
		return nil, fmt.Errorf("open authorized file: %w", err)
	}
	defer func() { _ = opened.File.Close() }()
	info, err := opened.File.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file: %s", name)
	}

	content, err := io.ReadAll(opened.File)
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}

	return content, nil
}

// resolvePath resolves a user-provided path against the working directory.
// Expands ~ to $HOME, resolves relative paths against workDir,
// and returns absolute paths as-is (after cleanup).
func resolvePath(workDir, path string) string {
	if strings.HasPrefix(path, "~/") || path == "~" {
		home, _ := coagenthome.UserHome()
		if home != "" {
			path = filepath.Join(home, path[1:])
		}
	}

	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}

	return filepath.Join(workDir, path)
}
