package coagenthome

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	// DirName is the coagent home directory name under the user's home.
	DirName = ".coagent"

	ConfigFileName       = "config.yaml"
	SecretsFileName      = "secrets"
	SocketFileName       = "daemon.sock"
	LockFileName         = "daemon.lock"
	DBFileName           = "daemon.db"
	PendingApplyFileName = "pending-apply.json"

	ProjectsDirName     = "projects"
	BinDirName          = "bin"
	CacheDirName        = "cache"
	CatalogDirName      = "catalog"
	MarketplacesDirName = "marketplaces"

	// TelegramServiceFilePattern is an fmt pattern keyed by target chat ID.
	TelegramServiceFilePattern = "tg-service-%d.json"
)

var (
	overrideMu  sync.RWMutex
	overrideDir string
	overrideSet bool

	startupUserHome = resolveStartupUserHome()
)

// UserHome returns the current user's home directory.
func UserHome() (string, error) {
	if dir, ok := overridden(); ok {
		if dir == "" {
			return "", errors.New("home directory override is empty")
		}

		if runningUnderGoTest() && insideDirectory(dir, startupUserHome) {
			return "", errors.New(
				"refusing to use the inherited user home or a path beneath it in a test: isolate HOME under t.TempDir()",
			)
		}

		return dir, nil
	}

	userHome, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}

	if runningUnderGoTest() && insideDirectory(userHome, startupUserHome) {
		return "", errors.New(
			"refusing to use the inherited user home or a path beneath it in a test: isolate HOME under t.TempDir()",
		)
	}

	return userHome, nil
}

// Dir returns the coagent home directory (~/.coagent).
func Dir() (string, error) {
	userHome, err := UserHome()
	if err != nil {
		return "", err
	}

	return filepath.Join(userHome, DirName), nil
}

// Join returns a path under the coagent home.
func Join(elem ...string) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}

	return filepath.Join(append([]string{dir}, elem...)...), nil
}

// Override forces UserHome to return dir — or to fail when dir is empty —
// until the returned restore func runs. Test-only; not for parallel tests.
func Override(dir string) func() {
	prevDir, prevSet := swap(dir, true)

	return func() {
		swap(prevDir, prevSet)
	}
}

func overridden() (string, bool) {
	overrideMu.RLock()
	defer overrideMu.RUnlock()

	return overrideDir, overrideSet
}

func swap(dir string, set bool) (string, bool) {
	overrideMu.Lock()
	defer overrideMu.Unlock()

	prevDir, prevSet := overrideDir, overrideSet
	overrideDir, overrideSet = dir, set

	return prevDir, prevSet
}

func resolveStartupUserHome() string {
	dir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	return dir
}

func runningUnderGoTest() bool {
	name := filepath.Base(os.Args[0])
	name = strings.TrimSuffix(name, ".exe")

	return strings.HasSuffix(name, ".test")
}

func insideDirectory(path, root string) bool {
	path = canonicalPath(path)
	root = canonicalPath(root)

	if path == "" || root == "" {
		return false
	}

	if path == root {
		return true
	}

	if filepath.Dir(root) == root {
		return false
	}

	rel, err := filepath.Rel(root, path)

	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func canonicalPath(path string) string {
	if path == "" {
		return ""
	}

	abs, err := filepath.Abs(path)
	if err == nil {
		path = abs
	}

	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		path = resolved
	}

	return filepath.Clean(path)
}
