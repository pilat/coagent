package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
)

const (
	defaultProjectsRoot = "~/" + coagenthome.DirName + "/" + coagenthome.ProjectsDirName
	maxProjectNameRunes = 64
)

// CreateProject provisions (get-or-create, idempotent) a folder-project named
// `data.Name` under the configured projects root and returns its id and path.
// mkdir happens daemon-side so managers never touch the filesystem; a repeat call
// with the same name recreates a manually-deleted folder and returns the same id.
func (c *controller) CreateProject(
	ctx context.Context,
	data controllerapi.ProjectCreateData,
) (*controllerapi.ProjectCreateResultData, error) {
	name, err := sanitizeProjectName(data.Name)
	if err != nil {
		return nil, err
	}

	path := filepath.Join(resolveProjectsRoot(c.cfg.UnifiedConfig), name)

	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, fmt.Errorf("create project dir: %w", err)
	}

	projectID, err := c.svc.GetOrCreateProject(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("resolve project: %w", err)
	}

	return &controllerapi.ProjectCreateResultData{
		ID:   projectID,
		Name: name,
		Path: path,
	}, nil
}

// ListRecentProjects returns the folder-projects under the configured root,
// newest activity first, for the /new picker.
func (c *controller) ListRecentProjects(ctx context.Context) (*controllerapi.ProjectListResultData, error) {
	recent, err := c.svc.ListRecentProjects(ctx, resolveProjectsRoot(c.cfg.UnifiedConfig))
	if err != nil {
		return nil, fmt.Errorf("list recent projects: %w", err)
	}

	projects := make([]controllerapi.RecentProjectInfo, len(recent))
	for i, r := range recent {
		projects[i] = controllerapi.RecentProjectInfo{
			ID:           r.ID,
			Name:         r.Name,
			Path:         r.WorkDir,
			LastActivity: r.LastActivity,
		}
	}

	return &controllerapi.ProjectListResultData{Projects: projects}, nil
}

// resolveProjectsRoot returns the folder-project root: configured projects_root
// or ~/.coagent/projects, with a leading ~ expanded and the path absolutized so
// it matches the abs work_dir GetOrCreateProject stores (a relative root would
// filter out every project). Nil-safe — a missing config.yaml is valid.
func resolveProjectsRoot(uc *config.UnifiedConfig) string {
	root := defaultProjectsRoot
	if uc != nil && strings.TrimSpace(uc.ProjectsRoot) != "" {
		root = strings.TrimSpace(uc.ProjectsRoot)
	}

	if strings.HasPrefix(root, "~/") {
		if home, err := coagenthome.UserHome(); err == nil {
			root = filepath.Join(home, root[2:])
		}
	}

	if abs, err := filepath.Abs(root); err == nil {
		return abs
	}

	return filepath.Clean(root)
}

// sanitizeProjectName trims and validates a user-supplied name for use as one
// path segment. Blacklist (not whitelist) so cyrillic and other non-ASCII names
// pass; the cap counts runes (not bytes) so ~64 cyrillic chars are admitted. Each
// rejection names the violated rule — the text is shown to the user in Telegram.
func sanitizeProjectName(raw string) (string, error) {
	name := strings.TrimSpace(raw)

	if name == "" {
		return "", errors.New("project name is empty")
	}

	if strings.ContainsAny(name, "/\\\x00") {
		return "", errors.New(`project name must not contain "/", "\", or a NUL byte`)
	}

	if strings.HasPrefix(name, ".") {
		return "", errors.New(`project name must not start with "."`)
	}

	if utf8.RuneCountInString(name) > maxProjectNameRunes {
		return "", fmt.Errorf("project name must be at most %d characters", maxProjectNameRunes)
	}

	return name, nil
}
