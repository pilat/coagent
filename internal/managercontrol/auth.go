package managercontrol

import (
	"context"
	"errors"
	"fmt"
	"maps"

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

	record, err := s.backend.GetSession(ctx, sessionID)
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

func (s *service) authorizeAttributeUpdate(
	ctx context.Context,
	managerID string,
	data *controllerapi.SessionSetAttributesData,
) error {
	if err := requireManagerIdentity(managerID); err != nil {
		return err
	}

	record, err := s.backend.GetSession(ctx, data.SessionID)
	if err != nil || record == nil {
		return fmt.Errorf("session %d not found", data.SessionID)
	}

	owner, _ := record.Attributes[controllerapi.SessionAttributeManagerID].(string)
	if owner == "" {
		return fmt.Errorf("session %d has no claimable manager owner", data.SessionID)
	}

	if owner != managerID {
		return fmt.Errorf("session %d belongs to another manager", data.SessionID)
	}

	data.Attributes = maps.Clone(data.Attributes)
	if data.Attributes == nil {
		data.Attributes = make(map[string]any)
	}

	data.Attributes[controllerapi.SessionAttributeManagerID] = managerID

	return nil
}
