package progressruntime

import (
	"context"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
)

func (r *runtime) ReconcileOutputReadiness(ctx context.Context, outputID int64) error {
	readiness, err := r.sessionStore.OutputReadiness(ctx, outputID)
	if errors.Is(err, sessionstore.ErrNoOutput) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("load output readiness: %w", err)
	}

	if !readiness.Ready || r.hasActiveLoop(readiness.SessionID) {
		return nil
	}

	r.publish(readiness.SessionID, sessionevent.Notification{
		Type: sessionevent.NotifyStateChanged, Status: controllerapi.StateIdle, Reason: readiness.Reason,
	})

	return nil
}

func (r *runtime) ReconcileLatestReadiness(ctx context.Context, sessionID int64) {
	outputID, err := r.sessionStore.LatestReleasingOutputID(ctx, sessionID)
	if err == nil {
		_ = r.ReconcileOutputReadiness(ctx, outputID)
	}
}
