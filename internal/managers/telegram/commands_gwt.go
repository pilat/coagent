package telegram

import (
	"context"
	"strconv"
	"strings"

	"github.com/pilat/coagent/internal/controllerapi"
)

// parseGwtCommand recognizes /gwt <name>. The name is the whole trimmed
// remainder; a bare /gwt has no name and reports ok with an empty name so the
// handler can explain the usage.
func parseGwtCommand(text string) (string, bool) {
	if text == "/gwt" {
		return "", true
	}

	if arg, ok := strings.CutPrefix(text, "/gwt "); ok {
		return strings.TrimSpace(arg), true
	}

	return "", false
}

func (m *Manager) handleGWT(ctx context.Context, sessionID, threadID int64, name string) {
	if name == "" {
		_, _ = m.sendMessage(ctx, "Usage: /gwt <name> — fork this project into a git worktree.", nil, threadID)
		return
	}

	workDir, ok := m.workDirBySessionID(ctx, sessionID)
	if !ok || workDir == "" {
		_, _ = m.sendMessage(ctx, "❌ Could not resolve this session's directory.", nil, threadID)
		return
	}

	// The pending bubble becomes the outcome: one message per attempt, never
	// a dangling "Creating..." on either path.
	progressID, _ := m.sendMessage(ctx, "🌿 Creating worktree “"+name+"”...", nil, threadID)

	newSessionID, err := m.controller.CreateSession(ctx, controllerapi.SessionCreateData{
		WorkDir:      workDir,
		WorktreeName: name,
		Attributes:   map[string]any{"channel": telegramChannel},
	})
	if err != nil {
		m.reportGwtOutcome(ctx, progressID, "❌ /gwt failed: "+err.Error(), threadID)
		return
	}

	m.reportGwtOutcome(ctx, progressID, m.gwtCreatedText(ctx, newSessionID, name), threadID)
}

// reportGwtOutcome edits the pending “Creating...” bubble into the result,
// falling back to a fresh message when the bubble was never delivered or the
// edit is rejected — /gwt must always report its outcome.
func (m *Manager) reportGwtOutcome(ctx context.Context, progressID int64, text string, threadID int64) {
	if progressID > 0 {
		if err := m.editMessageText(ctx, progressID, text, nil); err == nil {
			return
		}
	}

	_, _ = m.sendMessage(ctx, text, nil, threadID)
}

// gwtCreatedText names the fork by its registered project ("<repo>/<branch>").
// The project name lives on the session, so it comes from a controller lookup;
// the requested name is the honest fallback when the lookup fails.
func (m *Manager) gwtCreatedText(ctx context.Context, sessionID int64, name string) string {
	label := name

	if sessions, err := m.controller.ListSessions(ctx); err == nil {
		for _, s := range sessions {
			if s.ID == sessionID && s.ProjectName != "" {
				label = s.ProjectName
				break
			}
		}
	}

	return "🌿 Worktree created: " + label + " (#" + strconv.FormatInt(sessionID, 10) + ")"
}

// workDirBySessionID reads the cached work dir for a session, falling back to a
// controller lookup when the cache has not been populated yet.
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

	for _, s := range sessions {
		if s.ID == sessionID && s.WorkDir != "" {
			m.setWorkDir(sessionID, s.WorkDir)

			return s.WorkDir, true
		}
	}

	return "", false
}
