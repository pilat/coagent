package telegram

import (
	"context"
	"path/filepath"
	"slices"
	"strings"

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

func labelFromWorkDir(workDir string) string {
	parts := strings.Split(workDir, "/")
	for _, v := range slices.Backward(parts) {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}

	return workDir
}

func makeLabels(sessions []controllerapi.SessionInfo) map[int64]string {
	lastParts := make(map[string][]int64)
	labels := make(map[int64]string, len(sessions))

	for _, s := range sessions {
		last := labelFromWorkDir(s.WorkDir)
		lastParts[last] = append(lastParts[last], s.ID)
	}

	for _, s := range sessions {
		last := labelFromWorkDir(s.WorkDir)
		if len(lastParts[last]) == 1 {
			labels[s.ID] = last
			continue
		}

		parent := filepath.Base(filepath.Dir(s.WorkDir))
		if parent == "." || parent == "/" || parent == "" {
			labels[s.ID] = last
			continue
		}

		labels[s.ID] = parent + "/" + last
	}

	return labels
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
	assistantID := m.assistantRootID
	m.mu.RUnlock()

	id := sn.SessionID
	if id == assistantID {
		id = 0
	}

	if delivery != nil && delivery.Focus() == id {
		_ = m.sendTyping(ctx)
	}
}
