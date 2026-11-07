package telegram

import (
	"context"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionevent"
)

func isActiveSession(s controllerapi.SessionInfo) bool {
	return s.KilledAt == nil && s.Status != "terminating" && s.Status != "killed"
}

func filterActiveSessions(sessions []controllerapi.SessionInfo) []controllerapi.SessionInfo {
	active := make([]controllerapi.SessionInfo, 0, len(sessions))

	for _, s := range sessions {
		if isActiveSession(s) {
			active = append(active, s)
		}
	}

	return active
}

func (m *Manager) setWorkDir(sessionID int64, workDir string) {
	m.mu.Lock()
	m.workDirs[sessionID] = workDir
	m.mu.Unlock()
}

func (m *Manager) wakeDelivery() {
	m.mu.RLock()
	delivery := m.delivery
	m.mu.RUnlock()

	if delivery != nil {
		delivery.Wake()
	}
}

func (m *Manager) handleNotification(ctx context.Context, sn controllerapi.SessionNotification) {
	m.wakeDelivery()

	if sn.Notification.Type != sessionevent.NotifyHeartbeat {
		return
	}

	m.mu.RLock()
	delivery := m.delivery
	m.mu.RUnlock()

	if delivery != nil && sn.SessionID > 0 && delivery.Focus() == sn.SessionID {
		_ = m.sendTyping(ctx)
	}
}
