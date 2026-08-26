package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionevent"
)

func (m *Manager) getSessionByTopicID(topicID int64) (int64, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	sessionID, ok := m.topicToSession[topicID]

	return sessionID, ok
}

func isActiveSession(s controllerapi.SessionInfo) bool {
	return s.KilledAt == nil && s.Status != "terminating"
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

// resolveSessionByTopicID returns a mapped session for topicID.
// On cache miss it falls back to session attributes lookup and rehydrates in-memory maps.
func (m *Manager) resolveSessionByTopicID(ctx context.Context, topicID int64) (int64, bool) {
	if sessionID, ok := m.getSessionByTopicID(topicID); ok {
		return sessionID, true
	}

	sessions, err := m.controller.ListSessions(ctx)
	if err != nil {
		return 0, false
	}

	for _, s := range m.filterOwnedActiveSessions(sessions) {
		attrTopicID, ok := topicIDFromAttributes(s.Attributes)
		if !ok || attrTopicID != topicID {
			continue
		}

		m.registerTopic(s.ID, topicID)

		if s.WorkDir != "" {
			m.setWorkDir(s.ID, s.WorkDir)
		}

		return s.ID, true
	}

	return 0, false
}

func (m *Manager) getTopicBySessionID(sessionID int64) (int64, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	topicID, ok := m.sessionToTopic[sessionID]

	return topicID, ok
}

func (m *Manager) registerTopic(sessionID, topicID int64) {
	m.mu.Lock()
	m.sessionToTopic[sessionID] = topicID
	m.topicToSession[topicID] = sessionID
	m.mu.Unlock()
}

func (m *Manager) unregisterTopic(sessionID int64) {
	m.mu.Lock()

	topicID, ok := m.sessionToTopic[sessionID]
	if ok {
		delete(m.topicToSession, topicID)
	}

	delete(m.sessionToTopic, sessionID)
	m.mu.Unlock()
}

// remapTopic moves an existing topic to a replacement session in one critical
// section. The durable attribute write must succeed before this is called.
func (m *Manager) remapTopic(oldSessionID, newSessionID, topicID int64, workDir string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.sessionToTopic, oldSessionID)
	delete(m.workDirs, oldSessionID)
	m.sessionToTopic[newSessionID] = topicID
	m.topicToSession[topicID] = newSessionID
	m.workDirs[newSessionID] = workDir
}

func (m *Manager) setWorkDir(sessionID int64, workDir string) {
	m.mu.Lock()
	m.workDirs[sessionID] = workDir
	m.mu.Unlock()
}

func (m *Manager) deleteWorkDir(sessionID int64) {
	m.mu.Lock()
	delete(m.workDirs, sessionID)
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

func (m *Manager) reconcileOnStartup(ctx context.Context) error {
	m.mu.Lock()
	m.sessionToTopic = make(map[int64]int64)
	m.topicToSession = make(map[int64]int64)
	m.workDirs = make(map[int64]string)
	m.mu.Unlock()

	sessions, err := m.controller.ListSessions(ctx)
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}

	for _, s := range m.filterOwnedActiveSessions(sessions) {
		m.setWorkDir(s.ID, s.WorkDir)

		if topicID, ok := topicIDFromAttributes(s.Attributes); ok {
			exists, err := m.forumTopicExists(ctx, topicID)
			if err != nil {
				return fmt.Errorf("verify topic for session %d: %w", s.ID, err)
			}

			if exists {
				m.registerTopic(s.ID, topicID)
				continue
			}
		}

		// Lifecycle rows own remote topic creation; startup may rehydrate a
		// proven binding but requests a missing surface through the outbox.
		queue, ok := m.controller.(controllerapi.OutputQueueController)
		if !ok {
			continue
		}

		binding := "none"
		if topicID, found := topicIDFromAttributes(s.Attributes); found {
			binding = strconv.FormatInt(topicID, 10)
		}

		if err := queue.RepairSessionSurface(ctx, s.ID, binding); err != nil {
			return fmt.Errorf("enqueue topic repair for session %d: %w", s.ID, err)
		}

		m.wakeDelivery()
	}

	return nil
}

func topicIDFromAttributes(attrs map[string]any) (int64, bool) {
	if attrs == nil {
		return 0, false
	}

	raw, ok := attrs["telegram_topic_id"]
	if !ok || raw == nil {
		return 0, false
	}

	switch v := raw.(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case float64:
		return int64(v), true
	case json.Number:
		id, err := v.Int64()
		return id, err == nil
	default:
		return 0, false
	}
}

// handleNotification treats observer events as hints only: ordinary output is
// rendered from the durable outbox, so a notification at most wakes the worker.
func (m *Manager) handleNotification(ctx context.Context, sn controllerapi.SessionNotification) {
	n := sn.Notification

	switch n.Type {
	case sessionevent.NotifySessionCreated,
		sessionevent.NotifySessionCleared,
		sessionevent.NotifyMessage,
		sessionevent.NotifyWaiting,
		sessionevent.NotifyInputReceived:
		m.wakeDelivery()
	case sessionevent.NotifyStateChanged:
		if n.Reason == "killed" {
			m.wakeDelivery()
		}
	case sessionevent.NotifyHeartbeat:
		// Typing stays on the ephemeral activity channel.
		if topicID, ok := m.getTopicBySessionID(sn.SessionID); ok {
			_ = m.sendTyping(ctx, topicID)
		}
	case sessionevent.NotifySecretRequest, sessionevent.NotifySecretResolved:
		// A masked prompt needs a terminal. Telegram sessions never carry the
		// tool that raises this, so reaching here would be a routing bug.
	}
}

func (m *Manager) wakeDelivery() {
	if m.delivery != nil {
		m.delivery.Wake()
	}
}
