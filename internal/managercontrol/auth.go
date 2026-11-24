package managercontrol

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/sessionstore"
)

func requireManagerIdentity(managerID string) error {
	if managerID == "" {
		return errors.New("controller has no manager identity")
	}

	return nil
}

func (s *service) requireRoot(ctx context.Context, sessionID int64) (*sessionstore.SessionRecord, error) {
	record, err := s.store.GetSession(ctx, sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("session %d not found", sessionID)
	}

	if err != nil {
		return nil, fmt.Errorf("load session %d: %w", sessionID, err)
	}

	if record.ParentID != 0 {
		return nil, fmt.Errorf("session %d is not a root session", sessionID)
	}

	return record, nil
}
