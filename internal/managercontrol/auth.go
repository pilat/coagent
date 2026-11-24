package managercontrol

import (
	"context"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionstore"
)

func requireManagerIdentity(managerID string) error {
	if managerID == "" {
		return errors.New("controller has no manager identity")
	}

	return nil
}

func (s *service) managerRoot(ctx context.Context, sessionID int64) (*sessionstore.SessionRecord, error) {
	record, err := s.store.GetSession(ctx, sessionID)
	if err != nil || record == nil {
		return nil, fmt.Errorf("session %d not found", sessionID)
	}

	if record.ParentID != 0 || !controllerapi.ManagerRoot(record.Attributes) {
		return nil, fmt.Errorf("session %d is not a root session", sessionID)
	}

	return record, nil
}
