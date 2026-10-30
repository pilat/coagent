package assistant

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/projectpath"
)

type projectChoice struct {
	id         int64
	path, name string
}

type projectEntry struct {
	Project
	id      int64
	workDir string
}

// Projects lists the catalog, most recently active first.
func (s *Store) Projects(ctx context.Context) ([]Project, error) {
	entries, err := s.catalog(ctx)
	if err != nil {
		return nil, err
	}

	records, err := s.sessions.ListSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list project activity: %w", err)
	}

	projects := make([]Project, 0, len(entries))

	for _, entry := range entries {
		for _, record := range records {
			if record.ProjectID == entry.id && record.UpdatedAt.After(entry.Activity) {
				entry.Activity = record.UpdatedAt
			}
		}

		projects = append(projects, entry.Project)
	}

	slices.SortStableFunc(projects, func(a, b Project) int {
		return cmp.Or(b.Activity.Compare(a.Activity), strings.Compare(a.Key, b.Key))
	})

	return projects, nil
}

func (s *Store) catalog(ctx context.Context) ([]projectEntry, error) {
	rows, err := s.sessions.ListProjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("list assistant projects: %w", err)
	}

	worktrees, err := coagenthome.Join(coagenthome.WorktreesDirName)
	if err != nil {
		return nil, fmt.Errorf("resolve assistant worktrees: %w", err)
	}

	var entries []projectEntry

	names := map[string]int{}

	for _, row := range rows {
		if row.Hidden || row.WorkDir == worktrees ||
			strings.HasPrefix(row.WorkDir, worktrees+string(filepath.Separator)) {
			continue
		}

		info, err := os.Stat(row.WorkDir)
		if err != nil || !info.IsDir() {
			continue
		}

		entries = append(entries, projectEntry{
			Name: row.Name, Key: row.Name, Place: projectPlace(row.WorkDir), id: row.ID, workDir: row.WorkDir,
		})
		names[row.Name]++
	}

	for i := range entries {
		if names[entries[i].Name] > 1 {
			entries[i].Key = catalogPath(entries[i].workDir)
			if entries[i].Place == "scratch" {
				entries[i].Place += " " + entries[i].Key
			}
		}
	}

	return entries, nil
}

func (s *Store) projectID(ctx context.Context, key string) (int64, error) {
	entries, err := s.catalog(ctx)
	if err != nil {
		return 0, err
	}

	for _, entry := range entries {
		if entry.Key == key {
			return entry.id, nil
		}
	}

	return 0, fmt.Errorf("no project %q: projects lists the keys", key)
}

func (s *Store) openTarget(ctx context.Context, raw string, worktree bool) (projectChoice, error) {
	key := strings.TrimSpace(raw)

	entries, err := s.catalog(ctx)
	if err != nil {
		return projectChoice{}, err
	}

	path, isPath := projectPathArg(key)
	for _, entry := range entries {
		if (isPath && entry.workDir == path) || (!isPath && entry.Key == key) {
			return projectChoice{id: entry.id, path: entry.workDir, name: entry.Name}, nil
		}
	}

	if isPath {
		return projectChoice{}, fmt.Errorf("no project at %s", raw)
	}

	for _, entry := range entries {
		if entry.Name == key {
			return projectChoice{}, fmt.Errorf("project name %q is ambiguous; open it by its path from projects", key)
		}
	}

	if worktree {
		return projectChoice{}, fmt.Errorf("worktree needs an existing project: %s", raw)
	}

	name, err := projectpath.SanitizeName(key)
	if err != nil {
		return projectChoice{}, fmt.Errorf("resolve assistant project: %w", err)
	}

	path = filepath.Join(s.projectsRoot, name)
	if err := os.MkdirAll(path, 0o755); err != nil {
		return projectChoice{}, fmt.Errorf("create assistant work directory: %w", err)
	}

	id, err := s.sessions.GetOrCreateProject(ctx, path)
	if err != nil {
		return projectChoice{}, fmt.Errorf("create assistant work project: %w", err)
	}

	return projectChoice{id: id, path: path, name: name}, nil
}

// projectPathArg accepts the catalog's path key, so a name shared by several
// projects can still be opened.
func projectPathArg(raw string) (string, bool) {
	if strings.HasPrefix(raw, "~/") {
		home, err := coagenthome.UserHome()
		if err != nil {
			return "", false
		}

		return filepath.Join(home, raw[2:]), true
	}

	if filepath.IsAbs(raw) {
		return filepath.Clean(raw), true
	}

	return "", false
}

// projectPlace tells the assistant whether a session works in the user's code or in a
// project Coagent made for it, so a related-looking name does not pull work into a repo.
func projectPlace(workDir string) string {
	if projects, err := coagenthome.Join(coagenthome.ProjectsDirName); err == nil &&
		(workDir == projects || strings.HasPrefix(workDir, projects+string(os.PathSeparator))) {
		return "scratch"
	}

	shown := catalogPath(workDir)
	if _, err := os.Stat(filepath.Join(workDir, ".git")); err == nil {
		return "repo " + shown
	}

	return "dir " + shown
}

func catalogPath(path string) string {
	if home, err := coagenthome.UserHome(); err == nil && strings.HasPrefix(path, home+string(filepath.Separator)) {
		return "~" + strings.TrimPrefix(path, home)
	}

	return path
}
