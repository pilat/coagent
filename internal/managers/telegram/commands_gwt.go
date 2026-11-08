package telegram

import (
	"context"
	"strings"

	"github.com/pilat/coagent/internal/controllerapi"
)

func parseGwtCommand(text string) (string, bool) {
	if text == commandGWT {
		return "", true
	}

	if arg, ok := strings.CutPrefix(text, commandGWT+" "); ok {
		return strings.TrimSpace(arg), true
	}

	return "", false
}

func (m *Manager) handleGWT(ctx context.Context, sessionID int64, name string) {
	if name == "" {
		m.reply(
			ctx,
			commandGWT,
			strings.ReplaceAll("Usage: /gwt <name> — fork this project into a git worktree.", "\n", " "),
		)

		return
	}

	workDir, ok := m.workDirBySessionID(ctx, sessionID)
	if !ok || workDir == "" {
		m.reply(ctx, commandGWT, strings.ReplaceAll("Could not resolve this session's directory.", "\n", " "))
		return
	}

	m.createAndSwitch(ctx, commandGWT, controllerapi.SessionCreateData{
		WorkDir: workDir, WorktreeName: name, Attributes: map[string]any{"channel": telegramChannel},
	})
}

func (m *Manager) workDirBySessionID(ctx context.Context, sessionID int64) (string, bool) {
	m.mu.RLock()
	workDir, ok := m.workDirs[sessionID]
	m.mu.RUnlock()

	if ok && workDir != "" {
		return workDir, true
	}

	sessions, err := m.controller.ListSessions(ctx)
	if err != nil {
		return "", false
	}

	for _, s := range m.filterOwnedActiveSessions(sessions) {
		if s.ID == sessionID && s.WorkDir != "" {
			m.setWorkDir(sessionID, s.WorkDir)

			return s.WorkDir, true
		}
	}

	return "", false
}
