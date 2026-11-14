package configops

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/logger"
)

// Pending identifies the rollback target and the session to notify on failed boot.
type Pending struct {
	// SessionID is notified only on rollback; zero means an unattended apply.
	SessionID int64  `json:"session_id"`
	BakPath   string `json:"bak_path"`
	NewHash   string `json:"new_config_sha256"`
}

// LoadPending reads the marker. A missing marker is nil, not an error: that is
// the normal boot.
//
//nolint:nilnil // "no marker" is the ordinary case, not a failure and not a value
func (s *svc) LoadPending() (*Pending, error) {
	data, err := os.ReadFile(s.markerPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("read pending-apply marker: %w", err)
	}

	var p Pending
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse pending-apply marker: %w", err)
	}

	return &p, nil
}

// ClearPending removes the marker p came from. A marker that no longer matches
// belongs to a newer apply, so it is left alone.
func (s *svc) ClearPending(p Pending) error {
	current, err := os.ReadFile(s.markerPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("read pending-apply marker: %w", err)
	}

	var decoded Pending
	if err := json.Unmarshal(current, &decoded); err != nil {
		return fmt.Errorf("parse pending-apply marker: %w", err)
	}

	if decoded != p {
		logger.Named("configops").Warn("pending_apply_marker_superseded",
			zap.Int64("session_id", p.SessionID))

		return nil
	}

	if err := os.Remove(s.markerPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear pending-apply marker: %w", err)
	}

	return nil
}

// Rollback restores the backup the marker names. A marker with no backup means
// the apply created the very first config file: removing it is the way back.
func (s *svc) Rollback(p Pending) error {
	if p.BakPath == "" {
		if err := os.Remove(s.configPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove config: %w", err)
		}

		return nil
	}

	data, err := os.ReadFile(p.BakPath)
	if err != nil {
		return fmt.Errorf("read backup %s: %w", p.BakPath, err)
	}

	if err := writeFileAtomic(s.configPath, data); err != nil {
		return fmt.Errorf("restore backup: %w", err)
	}

	return nil
}

// ConfigHash is the current file's sha256, or "" when there is no file.
func (s *svc) ConfigHash() (string, error) {
	data, err := os.ReadFile(s.configPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}

	if err != nil {
		return "", fmt.Errorf("read config: %w", err)
	}

	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:]), nil
}

func (s *svc) markerPath() string {
	return filepath.Join(filepath.Dir(s.configPath), coagenthome.PendingApplyFileName)
}

func writeMarker(path string, p Pending) error {
	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("encode pending-apply marker: %w", err)
	}

	if err := writeFileAtomic(path, data); err != nil {
		return fmt.Errorf("write pending-apply marker: %w", err)
	}

	return nil
}
