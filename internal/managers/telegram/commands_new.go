package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/pilat/coagent/internal/controllerapi"
)

// parseNewCommand recognizes the /new command in either form: bare (picker) or
// with a name argument (create). Reports ok=false for anything else. Names may
// contain spaces, so the whole remainder after "/new " is the argument.
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
		m.handleNewPicker(ctx, 0, 0)
		return
	}

	m.createAndLaunchProject(ctx, name)
}

// createAndLaunchProject provisions the named folder-project daemon-side (mkdir +
// get-or-create, idempotent) and opens a fresh session on it via the standard
// post-pick launch path. Shared by /new <name> and the picker's pick callback.
func (m *Manager) createAndLaunchProject(ctx context.Context, name string) {
	result, err := m.controller.CreateProject(ctx, controllerapi.ProjectCreateData{Name: name})
	if err != nil {
		_, _ = m.sendMessage(ctx, "❌ "+err.Error(), nil, m.serviceTopicID)
		return
	}

	m.handleLaunch(ctx, result.Path)
}

func (m *Manager) handleNewPicker(ctx context.Context, offset int, messageID int64) {
	result, err := m.controller.ListRecentProjects(ctx)
	if err != nil {
		m.sendOrEdit(ctx, messageID, "Failed to list projects: "+err.Error(), nil)
		return
	}

	if len(result.Projects) == 0 {
		m.sendOrEdit(ctx, messageID, "No projects yet. Create one with /new <name>.", nil)
		return
	}

	total := len(result.Projects)
	if offset < 0 || offset > total {
		offset = 0
	}

	start, end, hasMore := dirPageBounds(total, offset)
	keyboard := buildNewPickerKeyboard(result.Projects[start:end], offset, hasMore)

	pageInfo := ""
	if total > foldersPerPage {
		pageInfo = fmt.Sprintf(" (%d-%d of %d)", start+1, end, total)
	}

	m.sendOrEdit(ctx, messageID, "🗂 Projects"+pageInfo, &tgReplyMarkup{InlineKeyboard: keyboard})
}

// sendOrEdit posts a fresh service-topic message, or edits an existing one when
// messageID is set (pagination replaces the keyboard in place).
func (m *Manager) sendOrEdit(ctx context.Context, messageID int64, text string, markup *tgReplyMarkup) {
	if messageID > 0 {
		_ = m.editMessageText(ctx, messageID, text, markup)
		return
	}

	_, _ = m.sendMessage(ctx, text, markup, m.serviceTopicID)
}

// buildNewPickerKeyboard renders one already-paginated page: one project per row
// (name + relative age), then a nav row. Callbacks carry the numeric project id
// (newpick) and page offset (newpage) — never a name (a cyrillic name blows the
// 64-byte callback limit) and never a nav id (those reset on every /spawn).
func buildNewPickerKeyboard(projects []controllerapi.RecentProjectInfo, offset int, hasMore bool) [][]tgInlineButton {
	keyboard := make([][]tgInlineButton, 0, len(projects)+1)
	for _, p := range projects {
		keyboard = append(keyboard, []tgInlineButton{{
			Text:         p.Name + " · " + relativeAge(p.LastActivity),
			CallbackData: "newpick:" + strconv.FormatInt(p.ID, 10),
		}})
	}

	pagination := make([]tgInlineButton, 0, 2)

	if offset > 0 {
		pagination = append(pagination, tgInlineButton{
			Text:         "⬅️ Back",
			CallbackData: "newpage:" + strconv.Itoa(offset-foldersPerPage),
		})
	}

	if hasMore {
		pagination = append(pagination, tgInlineButton{
			Text:         "➡️ More",
			CallbackData: "newpage:" + strconv.Itoa(offset+foldersPerPage),
		})
	}

	if len(pagination) > 0 {
		keyboard = append(keyboard, pagination)
	}

	return keyboard
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

func (m *Manager) handleCallbackNewPick(ctx context.Context, cb *telegramCallbackData, action callbackAction) {
	m.answerCallback(ctx, cb.ID, "Opening...")

	// The callback carries only the project id, so re-list to resolve its name
	// (keeps stale picker keyboards valid — no per-picker state to go stale). Then
	// re-run the full /new flow, which recreates the folder if it was removed.
	result, err := m.controller.ListRecentProjects(ctx)
	if err != nil {
		_, _ = m.sendMessage(ctx, "❌ Failed to open project: "+err.Error(), nil, m.serviceTopicID)
		return
	}

	for _, p := range result.Projects {
		if p.ID == action.ProjectID {
			m.createAndLaunchProject(ctx, p.Name)
			return
		}
	}

	_, _ = m.sendMessage(ctx, "Project no longer available.", nil, m.serviceTopicID)
}

func (m *Manager) handleCallbackNewPage(ctx context.Context, cb *telegramCallbackData, action callbackAction) {
	m.answerCallback(ctx, cb.ID, "")
	m.handleNewPicker(ctx, action.Offset, cb.Message.MessageID)
}
