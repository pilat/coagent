package daemon

import (
	"context"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
)

// interruptedCallNotice is the typed failure a pending call is settled with
// after a daemon restart: the first attempt may have partially applied, so the
// call is never re-executed automatically — the model retries explicitly.
const interruptedCallNotice = "⚠️ This tool call was interrupted before completion (e.g., daemon restart) " +
	"and was not re-executed — the operation may have partially completed. " +
	"Check the current state before retrying."

// resolveInterruptedCalls settles every pending in-loop call left in transcripts
// by a restart. Re-executing any of them would apply an operation the model
// never saw complete, so each resolves as a typed failure and the model decides
// whether to retry. Runs before session resume so the loop never sees them.
func (s *svc) resolveInterruptedCalls(ctx context.Context) {
	log := logger.Ctx(ctx).Named("daemon.sweep")

	records, err := s.sessionStore.ListAllSessions(ctx)
	if err != nil {
		log.Error("list_sessions_for_interrupted_calls", zap.Error(err))

		return
	}

	closed := 0

	for _, rec := range records {
		if !orphanSweepCandidate(rec) {
			continue
		}

		// Nothing may open a runner before this pass returns, so a live loop is
		// a broken ordering contract, not a benign race.
		if s.HasActiveLoop(rec.ID) {
			continue
		}

		count, err := s.externalCalls.CloseInterrupted(ctx, rec)
		if err != nil {
			log.Error("close_interrupted_calls", zap.Int64("session_id", rec.ID), zap.Error(err))

			continue
		}

		closed += count
	}

	if closed > 0 {
		log.Warn("closed_interrupted_calls", zap.Int("calls", closed))
	}
}
