package assistant

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/projectpath"
	"github.com/pilat/coagent/internal/sessionstore"
)

// projectChoice is a resolved open target; id zero means a new scratch project at path.
type projectChoice struct {
	id   int64
	path string
	name string
}

// projectEntry is a listed project with the id the assistant never sees.
type projectEntry struct {
	Project
	id int64
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
		if order := b.Activity.Compare(a.Activity); order != 0 {
			return order
		}

		return strings.Compare(a.Key, b.Key)
	})

	return projects, nil
}

// catalog excludes hidden projects, missing folders and worktrees; a name shared by several uses its path as key.
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
	paths := map[int64]string{}

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
			Name: row.Name, Key: row.Name, Place: projectPlace(row.WorkDir), id: row.ID,
		})
		names[row.Name]++
		paths[row.ID] = row.WorkDir
	}

	for i := range entries {
		if names[entries[i].Name] > 1 {
			entries[i].Key = catalogPath(paths[entries[i].id])
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

func (s *Store) resolveProject(ctx context.Context, raw string) (projectChoice, error) {
	choice, err := s.lookupProject(ctx, raw)
	if err != nil || choice.id != 0 {
		return choice, err
	}

	if err := os.MkdirAll(choice.path, 0o755); err != nil {
		return projectChoice{}, fmt.Errorf("create assistant work directory: %w", err)
	}

	choice.id, err = s.sessions.GetOrCreateProject(ctx, choice.path)
	if err != nil {
		return projectChoice{}, fmt.Errorf("create assistant work project: %w", err)
	}

	return choice, nil
}

// lookupProject never creates anything, so open can be validated long before it runs.
func (s *Store) lookupProject(ctx context.Context, raw string) (projectChoice, error) {
	name := strings.TrimSpace(raw)

	projects, err := s.sessions.ListProjects(ctx)
	if err != nil {
		return projectChoice{}, fmt.Errorf("list assistant projects: %w", err)
	}

	if path, ok := projectPathArg(name); ok {
		for _, project := range projects {
			if project.WorkDir == path && !project.Hidden {
				return projectChoice{id: project.ID, path: project.WorkDir, name: project.Name}, nil
			}
		}

		return projectChoice{}, fmt.Errorf("no project at %s", raw)
	}

	var existing *sessionstore.ProjectRow

	for i := range projects {
		if projects[i].Name == name && !projects[i].Hidden {
			if existing != nil {
				return projectChoice{}, fmt.Errorf(
					"project name %q is ambiguous; open it by its path from projects",
					name,
				)
			}

			existing = &projects[i]
		}
	}

	if existing != nil {
		return projectChoice{id: existing.ID, path: existing.WorkDir, name: existing.Name}, nil
	}

	name, err = projectpath.SanitizeName(name)
	if err != nil {
		return projectChoice{}, fmt.Errorf("resolve assistant project: %w", err)
	}

	path := filepath.Join(s.projectsRoot, name)

	return projectChoice{path: path, name: name}, nil
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
