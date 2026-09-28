package daemon

import (
	"context"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionstore"
)

// resolveOrphanedCalls closes every external call whose producer did not survive
// the restart. Repair may never stub such a call, so without an owner to answer
// it the transcript can never be sent to a provider again.
func (s *svc) resolveOrphanedCalls(ctx context.Context) {
	log := logger.Ctx(ctx).Named("daemon.sweep")

	records, err := s.sessionStore.ListAllSessions(ctx)
	if err != nil {
		log.Error("list_sessions_for_orphaned_calls", zap.Error(err))

		return
	}

	closed := 0

	for _, rec := range records {
		if !orphanSweepCandidate(rec) {
			continue
		}

		// Nothing may open a runner before this pass returns, so a live loop is a
		// broken ordering contract, not a benign race — and it leaves an owner-less call.
		if s.HasActiveLoop(rec.ID) {
			log.Warn("orphan_sweep_skipped_running_session", zap.Int64("session_id", rec.ID))

			continue
		}

		count, err := s.externalCalls.CloseOrphans(ctx, rec)
		if err != nil {
			log.Error("close_orphaned_calls", zap.Int64("session_id", rec.ID), zap.Error(err))

			continue
		}

		closed += count
	}

	if closed > 0 {
		log.Warn("closed_orphaned_external_calls", zap.Int("calls", closed))
	}
}

// orphanSweepCandidate skips lifecycles this pass does not own: /stop settles a
// parked tree from the same durable set, and killed or finished is not resumed.
func orphanSweepCandidate(rec *sessionstore.SessionRecord) bool {
	if rec.KilledAt != nil {
		return false
	}

	return rec.Status == sessionstore.SessionStatusActive ||
		rec.Status == sessionstore.SessionStatusSuspended ||
		rec.Status == sessionstore.SessionStatusError
}

// orphanedCallNotice is the deliberate cancellation an unowned call is answered
// with — an owned outcome, not a repair stub.
func orphanedCallNotice(_ string) string {
	return "The daemon restarted while this call was out with the world, and its producer did not survive. " +
		"The outcome is unknown — check the current state before retrying."
}
