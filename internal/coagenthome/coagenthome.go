package coagenthome

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

	ProjectsDirName       = "projects"
	WorktreesDirName      = "worktrees"
	CacheDirName          = "cache"
	CatalogDirName        = "catalog"
	MarketplacesDirName   = "marketplaces"
	ProcessesDirName      = "processes"
	MCPAttachmentsDirName = "mcp-attachments"
)

var startupUserHome = resolveStartupUserHome()

// UserHome returns the current user's home directory.
func UserHome() (string, error) {
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

// ProcessProjectDir returns the process-artifact directory owned by one project.
func ProcessProjectDir(projectID int64) (string, error) {
	name, err := ProcessProjectDirName(projectID)
	if err != nil {
		return "", err
	}

	return Join(ProcessesDirName, name)
}

// ProcessProjectDirName returns the stable directory name beneath processes.
func ProcessProjectDirName(projectID int64) (string, error) {
	if projectID <= 0 {
		return "", fmt.Errorf("invalid process project id %d", projectID)
	}

	return "project-" + strconv.FormatInt(projectID, 10), nil
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
