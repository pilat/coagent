package builtin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pilat/coagent/internal/coagenthome"
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
