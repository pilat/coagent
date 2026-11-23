package telegram

import (
	"context"
	"strings"

	"github.com/pilat/coagent/api/coagent/v1"
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

	m.createAndSwitch(ctx, commandGWT, &coagentv1.CreateSessionRequest{
		WorkDir: workDir, WorktreeName: name,
	})
}

func (m *Manager) workDirBySessionID(ctx context.Context, sessionID int64) (string, bool) {
	m.mu.RLock()
	workDir, ok := m.workDirs[sessionID]
	m.mu.RUnlock()

	if ok && workDir != "" {
		return workDir, true
	}

	sessions, err := m.api.sessions.ListSessionDetails(ctx, &coagentv1.ListSessionDetailsRequest{})
	if err != nil {
		return "", false
	}

	for _, s := range m.filterOwnedActiveSessions(sessions.GetSessions()) {
		if s.GetId() == sessionID && s.GetWorkDir() != "" {
			m.setWorkDir(sessionID, s.GetWorkDir())

			return s.GetWorkDir(), true
		}
	}

	return "", false
}
