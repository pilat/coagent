package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionstore"
)

func (s *svc) GetOrCreateProject(ctx context.Context, workDir string) (int64, error) {
	id, err := s.store.GetOrCreateProject(ctx, workDir)
	if err != nil {
		return 0, fmt.Errorf("resolve project: %w", err)
	}

	return id, nil
}

func (s *svc) GetOrCreateNamedProject(ctx context.Context, workDir, name string) (int64, error) {
	id, err := s.store.GetOrCreateNamedProject(ctx, workDir, name)
	if err != nil {
		return 0, fmt.Errorf("resolve named project: %w", err)
	}

	return id, nil
}

func (s *svc) GetOrCreateHiddenProject(ctx context.Context, workDir string) (int64, error) {
	id, err := s.store.GetOrCreateHiddenProject(ctx, workDir)
	if err != nil {
		return 0, fmt.Errorf("resolve hidden project: %w", err)
	}

	return id, nil
}

// EnsureManagementRoot delegates the atomic management-root ensure to the
// session store; the manager-bound controller resolves the hidden project.
func (s *svc) EnsureManagementRoot(
	ctx context.Context,
	projectID int64,
	owner string,
	topicID int64,
	name, workDir string,
) (*sessionstore.SessionRecord, *sessionstore.OutputCommit, error) {
	record, commit, err := s.store.EnsureManagementRoot(ctx, projectID, owner, topicID, name, workDir)
	if err != nil {
		return nil, nil, fmt.Errorf("ensure management root: %w", err)
	}

	return record, commit, nil
}

// ListHiddenProjectDirs exposes hidden project work dirs so /spawn navigation
// omits their directories without inferring hidden state from a basename.
func (s *svc) ListHiddenProjectDirs(ctx context.Context) ([]string, error) {
	rows, err := s.store.ListProjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("list hidden projects: %w", err)
	}

	var dirs []string

	for _, row := range rows {
		if row.Hidden {
			dirs = append(dirs, row.WorkDir)
		}
	}

	return dirs, nil
}

func (s *svc) GetProjectWorkDir(ctx context.Context, projectID int64) (string, error) {
	workDir, err := s.store.GetProjectWorkDir(ctx, projectID)
	if err != nil {
		return "", fmt.Errorf("get project workdir: %w", err)
	}

	return workDir, nil
}

func (s *svc) GetProjectName(ctx context.Context, projectID int64) (string, error) {
	name, err := s.store.GetProjectName(ctx, projectID)
	if err != nil {
		return "", fmt.Errorf("get project name: %w", err)
	}

	return name, nil
}

// ListRecentProjects returns the folder-projects that are direct children of
// root, newest activity first. Only direct children: a pick reconstructs the
// folder as root/<name>, so a nested project (or a basename collision) would
// otherwise open the wrong directory. Projects with no sessions sort ahead of all
// others (a just-provisioned project tops the list); every tie breaks by id desc.
// root is expected pre-resolved (abs + clean) by the caller.
func (s *svc) ListRecentProjects(ctx context.Context, root string) ([]controllerapi.RecentProjectInfo, error) {
	rows, err := s.store.ListProjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}

	var (
		filtered []sessionstore.ProjectRow
		ids      []int64
	)

	for _, r := range rows {
		if filepath.Dir(r.WorkDir) != root || r.Hidden {
			continue
		}

		filtered = append(filtered, r)
		ids = append(ids, r.ID)
	}

	activity, err := s.store.LatestActivityByProject(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("latest activity: %w", err)
	}

	projects := make([]controllerapi.RecentProjectInfo, 0, len(filtered))

	for _, r := range filtered {
		p := controllerapi.RecentProjectInfo{ID: r.ID, Name: r.Name, Path: r.WorkDir}
		if t, ok := activity[r.ID]; ok {
			p.LastActivity = &t
		}

		projects = append(projects, p)
	}

	sortRecentProjects(projects)

	return projects, nil
}

// sortRecentProjects orders newest-activity-first; a nil LastActivity (no
// sessions) sorts ahead of any timestamped project, and every tie breaks by id
// descending.
func sortRecentProjects(projects []controllerapi.RecentProjectInfo) {
	sort.SliceStable(projects, func(i, j int) bool {
		a, b := projects[i], projects[j]

		if a.LastActivity == nil || b.LastActivity == nil {
			if (a.LastActivity == nil) != (b.LastActivity == nil) {
				return a.LastActivity == nil
			}

			return a.ID > b.ID
		}

		if a.LastActivity.Equal(*b.LastActivity) {
			return a.ID > b.ID
		}

		return a.LastActivity.After(*b.LastActivity)
	})
}
