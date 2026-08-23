package telegram

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pilat/coagent/internal/coagenthome"
)

type serviceTopicFile struct {
	Version   int           `json:"version,omitempty"`
	ManagerID string        `json:"manager_id,omitempty"`
	BotUserID int64         `json:"bot_user_id,omitempty"`
	Topology  forumTopology `json:"topology,omitempty"`
	ChatID    int64         `json:"chat_id,omitempty"`
	TopicID   int64         `json:"topic_id"`
}

func (m *Manager) serviceTopicPath() string {
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(m.id)))

	path, err := coagenthome.Join(fmt.Sprintf(coagenthome.TelegramServiceManagerFilePattern, digest))
	if err != nil {
		return ""
	}

	return path
}

func (m *Manager) legacyServiceTopicPath() string {
	path, err := coagenthome.Join(fmt.Sprintf(coagenthome.TelegramServiceFilePattern, m.effectiveChatID()))
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

	if payload.Version == 0 {
		if payload.ManagerID != "" || payload.BotUserID != 0 || payload.Topology != "" || payload.ChatID != 0 {
			return 0, errors.New("decode service topic file: invalid claimed legacy record")
		}

		return payload.TopicID, nil
	}

	if payload.Version != 1 || payload.ManagerID != m.id || payload.BotUserID != m.botUserID ||
		payload.Topology != m.target.topology ||
		payload.ChatID != m.effectiveChatID() {
		return 0, errors.New("service topic record identity does not match manager")
	}

	return payload.TopicID, nil
}

func (m *Manager) saveServiceTopicID(topicID int64) error {
	path := m.serviceTopicPath()
	if path == "" {
		return errors.New("resolve service topic path")
	}

	payload, err := json.Marshal(serviceTopicFile{
		Version: 1, ManagerID: m.id, BotUserID: m.botUserID, Topology: m.target.topology,
		ChatID: m.effectiveChatID(), TopicID: topicID,
	})
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

	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open service topic directory: %w", err)
	}
	defer dir.Close()

	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync service topic directory: %w", err)
	}

	return nil
}

func (m *Manager) ensureServiceTopic(ctx context.Context) (int64, error) {
	saved, err := m.loadServiceTopicID()
	if err != nil {
		return 0, err
	}

	if saved == 0 {
		saved, err = m.claimLegacyServiceTopic()
		if err != nil {
			return 0, err
		}
	}

	if saved > 0 {
		exists, err := m.forumTopicExists(ctx, saved)
		if err != nil {
			return 0, err
		}

		if exists {
			if err := m.saveServiceTopicID(saved); err != nil {
				return 0, err
			}

			return saved, nil
		}
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

func (m *Manager) claimLegacyServiceTopic() (int64, error) {
	legacy := m.legacyServiceTopicPath()
	if legacy == "" {
		return 0, nil
	}

	if err := m.validateLegacyServiceTopic(legacy); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}

		return 0, err
	}

	if err := os.Rename(legacy, m.serviceTopicPath()); err != nil {
		return 0, fmt.Errorf("claim legacy service topic record: %w", err)
	}

	if err := syncServiceTopicDirectory(m.serviceTopicPath()); err != nil {
		return 0, err
	}

	saved, err := m.loadServiceTopicID()
	if err != nil {
		return 0, err
	}

	return saved, nil
}

func (m *Manager) validateLegacyServiceTopic(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read legacy service topic file: %w", err)
	}

	var payload serviceTopicFile
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("decode legacy service topic file: %w", err)
	}

	if payload.Version != 0 || payload.TopicID <= 0 || payload.ManagerID != "" || payload.BotUserID != 0 ||
		payload.Topology != "" ||
		payload.ChatID != 0 {
		return errors.New("decode legacy service topic file: invalid legacy record")
	}

	return nil
}

func syncServiceTopicDirectory(path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open service topic directory: %w", err)
	}
	defer dir.Close()

	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync service topic directory: %w", err)
	}

	return nil
}

func (m *Manager) forumTopicExists(ctx context.Context, topicID int64) (bool, error) {
	if topicID <= 0 {
		return false, nil
	}

	err := m.editForumTopic(ctx, topicID)
	if err == nil {
		return true, nil
	}

	var apiErr *tgAPIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode == 400 &&
		strings.Contains(strings.ToLower(apiErr.Description), "message thread not found") {
		return false, nil
	}

	return false, fmt.Errorf("verify forum topic %d: %w", topicID, err)
}
