package daemon

import (
	"context"
	"errors"
	"fmt"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/admission"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/subagent"
)

// enqueueChild parks a background child that could not be admitted, preserving
// its initial messages so the prompt survives until a slot frees.
func (s *svc) enqueueChild(
	ctx context.Context,
	sessionID, parentID int64,
	workDir string,
	projectID int64,
) {
	s.childQueue.Push(queuedChild{
		sessionID: sessionID,
		parentID:  parentID,
		workDir:   workDir,
		projectID: projectID,
	})

	logger.Ctx(ctx).Named("daemon.admission").Info("subagent_queued", zap.Int64("child", sessionID))
}

// drainQueue starts one queued child whose parent now has capacity. Called after
// every slot release; ensureRunner re-checks admission (re-queueing on a race).
func (s *svc) drainQueue(ctx context.Context) {
	next, ok := s.childQueue.PopFirst(func(queued queuedChild) bool {
		return s.admit.CanAdmitChild(queued.parentID)
	})
	if !ok {
		return
	}

	// A child cascade-killed while parked (its link/session marked terminal by
	// killSubagent, but no runner existed to stop) must never be launched. It is
	// already removed from the queue above; skip it and try the next entry.
	terminated, err := s.childTerminated(ctx, next.sessionID)
	if err != nil {
		// Recursing on an unknown state would quietly drain the whole queue, so
		// park the entry instead and let the next slot release retry it.
		logger.Ctx(ctx).Named("daemon.admission").
			Error("queued_child_state_unknown", zap.Int64("child", next.sessionID), zap.Error(err))
		s.enqueueChild(ctx, next.sessionID, next.parentID, next.workDir, next.projectID)

		return
	}

	if terminated {
		logger.Ctx(ctx).Named("daemon.admission").Info("skip_killed_queued_child", zap.Int64("child", next.sessionID))
		s.drainQueue(ctx)

		return
	}

	err = s.ensureRunner(ctx, next.sessionID, next.workDir, next.projectID, nil)
	if errors.Is(err, admission.ErrNoCapacity) {
		// Admission lost a race — park it again for the next release.
		s.enqueueChild(ctx, next.sessionID, next.parentID, next.workDir, next.projectID)

		return
	}

	if err != nil {
		// Anything else is not a race, so re-parking would only spin on it.
		logger.Ctx(ctx).Named("daemon.admission").
			Error("queued_child_start_failed", zap.Int64("child", next.sessionID), zap.Error(err))
	}
}

// childTerminated reports whether a queued child was killed/terminalized before it
// got a runner (e.g. by cascadeKillChildren). Checked just before launch so a
// stale queue entry is never turned into a live runner. An unreadable ledger is
// neither answer, so the caller must defer the decision instead of guessing.
func (s *svc) childTerminated(ctx context.Context, childID int64) (bool, error) {
	link, err := s.links.GetLink(ctx, childID)
	if err != nil {
		return false, fmt.Errorf("queued child link %d: %w", childID, err)
	}

	if link != nil && (link.Terminal() || link.State == subagent.StateStopped) {
		return true, nil
	}

	rec, err := s.sessionStore.GetSession(ctx, childID)
	if err != nil {
		return false, fmt.Errorf("queued child session %d: %w", childID, err)
	}

	return rec.KilledAt != nil, nil
}
