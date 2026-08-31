package daemon

import (
	"context"
	"fmt"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionstore"
)

func (s *svc) ReconcileOutputReadiness(ctx context.Context, outputID int64) error {
	if s.progress == nil {
		return nil
	}

	if err := s.progress.ReconcileOutputReadiness(ctx, outputID); err != nil {
		return fmt.Errorf("reconcile output readiness: %w", err)
	}

	return nil
}

func ownerlessSession(record *sessionstore.SessionRecord) bool {
	owner, _ := record.Attributes[controllerapi.SessionAttributeManagerID].(string)

	return owner == ""
}

func (s *svc) reconcileLatestReadiness(ctx context.Context, sessionID int64) {
	if s.progress != nil {
		s.progress.ReconcileLatestReadiness(ctx, sessionID)
	}
}
