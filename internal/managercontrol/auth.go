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

func (s *service) requireOwnedSession(ctx context.Context, managerID string, sessionID int64) error {
	if err := requireManagerIdentity(managerID); err != nil {
		return err
	}

	record, err := s.store.GetSession(ctx, sessionID)
	if err != nil || record == nil {
		return fmt.Errorf("session %d not found", sessionID)
	}

	owner, _ := record.Attributes[controllerapi.SessionAttributeManagerID].(string)
	if owner != managerID {
		return fmt.Errorf("session %d belongs to another manager", sessionID)
	}

	return nil
}

func (s *service) canListSession(
	_ context.Context,
	managerID string,
	record *sessionstore.SessionRecord,
) bool {
	owner, _ := record.Attributes[controllerapi.SessionAttributeManagerID].(string)

	return owner == managerID && owner != ""
}
