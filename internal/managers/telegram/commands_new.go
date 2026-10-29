package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/pilat/coagent/internal/controllerapi"
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

	result, err := m.controller.CreateProject(ctx, controllerapi.ProjectCreateData{Name: name})
	if err != nil {
		m.reply(ctx, err.Error())
		return
	}

	m.handleLaunch(ctx, result.Path)
}

func (m *Manager) listProjects(ctx context.Context) {
	result, err := m.controller.ListRecentProjects(ctx)
	if err != nil {
		m.reply(ctx, "Failed to list projects: "+err.Error())
		return
	}

	if len(result.Projects) == 0 {
		m.reply(ctx, "No projects yet. Create one with /new <name>.")
		return
	}

	lines := []string{"Projects — open with /new <name>:"}
	for i, project := range result.Projects {
		lines = append(lines, fmt.Sprintf("%d. %s · %s", i+1, project.Name, relativeAge(project.LastActivity)))
	}

	_, _ = m.sendMessage(ctx, strings.Join(lines, "\n"))
}

func (m *Manager) handleSpawn(ctx context.Context, path string) {
	if path != "" {
		m.handleLaunch(ctx, path)
		return
	}

	result, err := m.controller.ListDir(ctx, controllerapi.FsListDirData{})
	if err != nil {
		m.reply(ctx, "Failed to list favorites: "+err.Error())
		return
	}

	lines := []string{"Usage: /spawn <path>."}
	if result != nil && len(result.Favorites) > 0 {
		lines = append(lines, "Favorites:")
		lines = append(lines, result.Favorites...)
	}

	_, _ = m.sendMessage(ctx, strings.Join(lines, "\n"))
}

func (m *Manager) handleLaunch(ctx context.Context, path string) {
	m.createAndSwitch(ctx, controllerapi.SessionCreateData{
		WorkDir: path, Attributes: map[string]any{"channel": telegramChannel},
	})
}

func (m *Manager) createAndSwitch(ctx context.Context, data controllerapi.SessionCreateData) {
	id, err := m.controller.CreateSession(ctx, data)
	if err != nil {
		m.reply(ctx, err.Error())
		return
	}

	m.switchChat(ctx, id)
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
