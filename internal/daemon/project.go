package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/pilat/coagent/internal/controllerapi"
)

// RecentProject is a daemon-managed folder-project with its most recent
// non-killed session activity (nil when it has no sessions yet).
type RecentProject struct {
	ID           int64
	Name         string
	WorkDir      string
	LastActivity *time.Time
}

// ListRecentProjects returns the folder-projects that are direct children of
// root, newest activity first. Only direct children: a pick reconstructs the
// folder as root/<name>, so a nested project (or a basename collision) would
// otherwise open the wrong directory. Projects with no sessions sort ahead of all
// others (a just-provisioned project tops the list); every tie breaks by id desc.
// root is expected pre-resolved (abs + clean) by the caller.
func (s *svc) ListRecentProjects(ctx context.Context, root string) ([]RecentProject, error) {
	rows, err := s.store.ListProjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}

	var (
		filtered []ProjectRow
		ids      []int64
	)

	for _, r := range rows {
		if filepath.Dir(r.WorkDir) != root || r.Name == controllerapi.CoagentSystemProjectName {
			continue
		}

		filtered = append(filtered, r)
		ids = append(ids, r.ID)
	}

	activity, err := s.sessionStore.LatestActivityByProject(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("latest activity: %w", err)
	}

	projects := make([]RecentProject, 0, len(filtered))

	for _, r := range filtered {
		p := RecentProject{ID: r.ID, Name: r.Name, WorkDir: r.WorkDir}
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
func sortRecentProjects(projects []RecentProject) {
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
