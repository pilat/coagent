//nolint:wrapcheck // Readiness preserves store sentinel errors.; nosemgrep: semgrep.coagent-no-preamble-before-package
package daemon

import (
	"context"
	"errors"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
)

func (s *svc) ReconcileOutputReadiness(ctx context.Context, outputID int64) error {
	store, ok := s.sessionStore.(sessionstore.ReadinessStore)
	if !ok {
		return nil
	}

	readiness, err := store.OutputReadiness(ctx, outputID)
	if errors.Is(err, sessionstore.ErrNoOutput) {
		return nil
	}

	if err != nil {
		return err
	}

	// A queued input that reactivated the root makes the delivered output's idle
	// obsolete; the live loop's own teardown publishes the next state instead.
	if readiness.Ready && s.HasActiveLoop(readiness.SessionID) {
		return nil
	}

	if !readiness.Ready {
		return nil
	}

	s.publish(readiness.SessionID, sessionevent.Notification{
		Type: sessionevent.NotifyStateChanged, Status: controllerapi.StateIdle, Reason: readiness.Reason,
	})

	return nil
}

func ownerlessSession(record *sessionstore.SessionRecord) bool {
	owner, _ := record.Attributes[controllerapi.SessionAttributeManagerID].(string)

	return owner == ""
}

func (s *svc) reconcileLatestReadiness(ctx context.Context, sessionID int64) {
	store, ok := s.sessionStore.(sessionstore.ReadinessStore)
	if !ok {
		return
	}

	outputID, err := store.LatestReleasingOutputID(ctx, sessionID)
	if err == nil {
		_ = s.ReconcileOutputReadiness(ctx, outputID)
	}
}
