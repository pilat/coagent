package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/pilat/coagent/api/coagent/v1"
)

func parseNewCommand(text string) (string, bool) {
	if text == "/new" {
		return "", true
	}

	if arg, ok := strings.CutPrefix(text, "/new "); ok {
		return strings.TrimSpace(arg), true
	}

	return "", false
}

func (m *Manager) handleNew(ctx context.Context, name string) {
	if name == "" {
		m.listProjects(ctx)
		return
	}

	result, err := m.api.discovery.CreateProject(ctx, &coagentv1.CreateProjectRequest{Name: name})
	if err != nil {
		m.reply(ctx, "/new", strings.ReplaceAll(err.Error(), "\n", " "))
		return
	}

	m.handleLaunch(ctx, "/new", result.GetPath())
}

func (m *Manager) listProjects(ctx context.Context) {
	result, err := m.api.discovery.ListProjects(ctx, &coagentv1.ListProjectsRequest{})
	if err != nil {
		m.reply(ctx, "/new", strings.ReplaceAll("Failed to list projects: "+err.Error(), "\n", " "))
		return
	}

	if len(result.GetProjects()) == 0 {
		m.reply(ctx, "/new", strings.ReplaceAll("No projects yet. Create one with /new <name>.", "\n", " "))
		return
	}

	lines := []string{"Projects — open with /new <name>:"}
	for i, project := range result.GetProjects() {
		lines = append(lines, fmt.Sprintf("%d. %s · %s", i+1, project.GetName(), relativeAge(projectActivity(project))))
	}

	m.reply(ctx, "/new", strings.Join(lines, "\n"))
}

func (m *Manager) handleSpawn(ctx context.Context, path string) {
	if path != "" {
		m.handleLaunch(ctx, "/spawn", path)
		return
	}

	favorites, err := m.api.discovery.ListSpawnFavorites(ctx, &coagentv1.ListSpawnFavoritesRequest{})
	if err != nil {
		m.reply(ctx, "/spawn", strings.ReplaceAll("Failed to list favorites: "+err.Error(), "\n", " "))
		return
	}

	lines := []string{"Usage: /spawn <path>."}
	if len(favorites.GetPaths()) > 0 {
		lines = append(lines, "Favorites:")
		lines = append(lines, favorites.GetPaths()...)
	}

	m.reply(ctx, "/spawn", strings.Join(lines, "\n"))
}

func (m *Manager) handleLaunch(ctx context.Context, command, path string) {
	m.createAndSwitch(ctx, command, &coagentv1.CreateSessionRequest{
		WorkDir: path,
	})
}

func (m *Manager) createAndSwitch(ctx context.Context, command string, data *coagentv1.CreateSessionRequest) {
	result, err := m.api.sessions.CreateSession(ctx, data)
	if err != nil {
		m.reply(ctx, command, strings.ReplaceAll(err.Error(), "\n", " "))
		return
	}

	m.switchChat(ctx, command, result.GetSessionId())
}

func relativeAge(t *time.Time) string {
	if t == nil {
		return "new"
	}

	return formatAge(time.Since(*t))
}

func formatAge(d time.Duration) string {
	switch {
	case d < time.Hour:
		return strconv.Itoa(max(int(d.Minutes()), 0)) + "m"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d"
	}
}

func projectActivity(project *coagentv1.Project) *time.Time {
	if project.GetLastActivity() == nil {
		return nil
	}

	activity := project.GetLastActivity().AsTime()

	return &activity
}
