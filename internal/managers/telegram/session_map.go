package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionevent"
)

type serviceTopicFile struct {
	TopicID int64 `json:"topic_id"`
}

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

	for _, s := range filterActiveSessions(sessions) {
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

func (m *Manager) serviceTopicPath() string {
	path, err := coagenthome.Join(fmt.Sprintf(coagenthome.TelegramServiceFilePattern, m.cfg.TargetChatID))
	if err != nil {
		return ""
	}

	return path
}

func (m *Manager) loadServiceTopicID() (int64, error) {
	path := m.serviceTopicPath()
	if path == "" {
		return 0, errors.New("resolve service topic path")
	}

	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}

	if err != nil {
		return 0, fmt.Errorf("read service topic file: %w", err)
	}

	var payload serviceTopicFile
	if err := json.Unmarshal(raw, &payload); err != nil {
		return 0, fmt.Errorf("decode service topic file: %w", err)
	}

	if payload.TopicID <= 0 {
		return 0, fmt.Errorf("decode service topic file: invalid topic id %d", payload.TopicID)
	}

	return payload.TopicID, nil
}

func (m *Manager) saveServiceTopicID(topicID int64) error {
	path := m.serviceTopicPath()
	if path == "" {
		return errors.New("resolve service topic path")
	}

	payload, err := json.Marshal(serviceTopicFile{TopicID: topicID})
	if err != nil {
		return fmt.Errorf("encode service topic file: %w", err)
	}

	if err := writeServiceTopicFile(path, payload); err != nil {
		return fmt.Errorf("write service topic file: %w", err)
	}

	return nil
}

// writeServiceTopicFile atomically replaces the tiny routing record. A crash
// before rename leaves the previous valid file in place instead of a truncated
// JSON document that would otherwise trigger a duplicate topic on restart.
func writeServiceTopicFile(path string, payload []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".telegram-service-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}

	tmpPath := tmp.Name()
	closed := false

	defer func() {
		if !closed {
			_ = tmp.Close()
		}

		_ = os.Remove(tmpPath)
	}()

	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod temporary file: %w", err)
	}

	if _, err := tmp.Write(payload); err != nil {
		return fmt.Errorf("write temporary file: %w", err)
	}

	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temporary file: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}

	closed = true

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace service topic file: %w", err)
	}

	return nil
}

func (m *Manager) ensureServiceTopic(ctx context.Context) (int64, error) {
	saved, err := m.loadServiceTopicID()
	if err != nil {
		return 0, err
	}

	if saved > 0 {
		return saved, nil
	}

	topicID, err := m.createForumTopic(ctx, m.cfg.ServiceTopicName, m.cfg.ServiceTopicIconEmojiID)
	if err != nil {
		return 0, err
	}

	if err := m.saveServiceTopicID(topicID); err != nil {
		persistErr := fmt.Errorf("persist service topic %d: %w", topicID, err)
		if cleanupErr := m.deleteForumTopic(ctx, topicID); cleanupErr != nil {
			return 0, errors.Join(
				persistErr,
				fmt.Errorf("delete unbound service topic %d: %w", topicID, cleanupErr),
			)
		}

		return 0, persistErr
	}

	return topicID, nil
}

func (m *Manager) createTopicForSession(
	ctx context.Context,
	sessionID int64,
	workDir, name string,
	attrs map[string]any,
) (int64, error) {
	topicName := name
	if topicName == "" {
		topicName = labelFromWorkDir(workDir)
	}

	// Rune-safe: byte-slicing a cyrillic name mid-rune yields invalid UTF-8 and
	// createForumTopic silently fails, leaving the session with no topic.
	if runes := []rune(topicName); len(runes) > maxTopicNameRunes {
		topicName = string(runes[:maxTopicNameRunes])
	}

	topicID, err := m.createForumTopic(ctx, topicName, m.cfg.SessionTopicIconEmojiID)
	if err != nil {
		return 0, err
	}

	updatedAttrs := maps.Clone(attrs)
	if updatedAttrs == nil {
		updatedAttrs = make(map[string]any)
	}

	updatedAttrs["telegram_topic_id"] = topicID

	if err := m.controller.SetSessionAttributes(ctx, controllerapi.SessionSetAttributesData{
		SessionID:  sessionID,
		Attributes: updatedAttrs,
	}); err != nil {
		persistErr := fmt.Errorf("persist topic %d for session %d: %w", topicID, sessionID, err)
		if cleanupErr := m.deleteForumTopic(ctx, topicID); cleanupErr != nil {
			return 0, errors.Join(persistErr, fmt.Errorf("delete unbound topic %d: %w", topicID, cleanupErr))
		}

		return 0, persistErr
	}

	// Persistence is now ahead of the in-memory projection.
	m.registerTopic(sessionID, topicID)
	m.setWorkDir(sessionID, workDir)

	return topicID, nil
}

func (m *Manager) deleteTopicForSession(ctx context.Context, sessionID int64) {
	topicID, ok := m.getTopicBySessionID(sessionID)
	if !ok {
		return
	}

	_ = m.deleteForumTopic(ctx, topicID)
	m.unregisterTopic(sessionID)
}

func (m *Manager) verifyTopicExists(ctx context.Context, topicID int64) bool {
	if topicID == 0 {
		return false
	}

	err := m.editForumTopic(ctx, topicID)

	return err == nil
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

	for _, s := range filterActiveSessions(sessions) {
		m.setWorkDir(s.ID, s.WorkDir)

		if topicID, ok := topicIDFromAttributes(s.Attributes); ok && m.verifyTopicExists(ctx, topicID) {
			m.registerTopic(s.ID, topicID)
			continue
		}

		if _, err := m.createTopicForSession(ctx, s.ID, s.WorkDir, s.Name, s.Attributes); err != nil {
			return fmt.Errorf("create topic for session %d: %w", s.ID, err)
		}
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

func (m *Manager) handleNotification(ctx context.Context, sn controllerapi.SessionNotification) {
	n := sn.Notification
	sessionID := sn.SessionID

	switch n.Type {
	case sessionevent.NotifySessionCreated:
		if topicID, ok := topicIDFromAttributes(n.Attributes); ok && m.verifyTopicExists(ctx, topicID) {
			m.registerTopic(sessionID, topicID)
			m.setWorkDir(sessionID, n.WorkDir)

			return
		}

		if _, err := m.createTopicForSession(ctx, sessionID, n.WorkDir, n.Name, n.Attributes); err != nil {
			logger.Ctx(ctx).Named("telegram").Error(
				"bind_session_topic",
				zap.Int64("session_id", sessionID),
				zap.Error(err),
			)
		}
	case sessionevent.NotifySessionCleared:
		m.handleSessionCleared(ctx, n)
	case sessionevent.NotifyMessage:
		topicID, ok := m.getTopicBySessionID(sessionID)
		if !ok {
			return
		}

		m.sendSessionNotification(ctx, sessionID, n.Type, n.Message, topicID)
	case sessionevent.NotifyWaiting:
		topicID, ok := m.getTopicBySessionID(sessionID)
		if !ok {
			return
		}

		m.sendSessionNotification(ctx, sessionID, n.Type, n.Message, topicID)
	case sessionevent.NotifyInputReceived:
		if n.Source == "user" {
			return
		}

		topicID, ok := m.getTopicBySessionID(sessionID)
		if !ok {
			return
		}

		prefix := "⏰ scheduled"
		if n.Source == "agent" {
			prefix = "📨 from agent"
		}

		m.sendSessionNotification(ctx, sessionID, n.Type, "["+prefix+"] "+n.Message, topicID)
	case sessionevent.NotifyHeartbeat:
		topicID, ok := m.getTopicBySessionID(sessionID)
		if !ok {
			return
		}

		_ = m.sendTyping(ctx, topicID)
	case sessionevent.NotifyStateChanged:
		if n.Status == controllerapi.StateIdle && n.Reason == "killed" {
			m.deleteTopicForSession(ctx, sessionID)
			m.deleteWorkDir(sessionID)
		}
	case sessionevent.NotifySecretRequest, sessionevent.NotifySecretResolved:
		// A masked prompt needs a terminal. Telegram sessions never carry the
		// tool that raises this, so reaching here would be a routing bug.
	}
}

func (m *Manager) sendSessionNotification(
	ctx context.Context,
	sessionID int64,
	eventType sessionevent.NotificationType,
	message string,
	topicID int64,
) {
	if _, err := m.sendMessage(ctx, message, nil, topicID); err != nil {
		logger.Ctx(ctx).Named("telegram").Error(
			"send_session_notification",
			zap.Int64("session_id", sessionID),
			zap.String("type", string(eventType)),
			zap.Error(err),
		)
	}
}

func (m *Manager) handleSessionCleared(ctx context.Context, n sessionevent.Notification) {
	oldTopicID, ok := m.getTopicBySessionID(n.OldSessionID)
	if !ok {
		return
	}

	updatedAttrs := maps.Clone(n.Attributes)
	if updatedAttrs == nil {
		updatedAttrs = make(map[string]any)
	}

	updatedAttrs["telegram_topic_id"] = oldTopicID

	if err := m.controller.SetSessionAttributes(ctx, controllerapi.SessionSetAttributesData{
		SessionID:  n.NewSessionID,
		Attributes: updatedAttrs,
	}); err != nil {
		logger.Ctx(ctx).Named("telegram").Error(
			"persist_cleared_session_topic",
			zap.Int64("old_session_id", n.OldSessionID),
			zap.Int64("new_session_id", n.NewSessionID),
			zap.Int64("topic_id", oldTopicID),
			zap.Error(err),
		)

		return
	}

	m.remapTopic(n.OldSessionID, n.NewSessionID, oldTopicID, n.WorkDir)
	_, _ = m.sendMessage(ctx, "🧹 Session cleared. Send a message to start fresh.", nil, oldTopicID)
}
