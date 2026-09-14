package daemon

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
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

		count, err := s.closeInterruptedCalls(ctx, rec)
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

// closeInterruptedCalls resolves a session's pending in-loop calls.
func (s *svc) closeInterruptedCalls(ctx context.Context, rec *sessionstore.SessionRecord) (int, error) {
	pending, err := s.storedInterruptedCalls(ctx, rec.ID)
	if err != nil || len(pending) == 0 {
		return 0, err
	}

	workDir, err := s.store.GetProjectWorkDir(ctx, rec.ProjectID)
	if err != nil {
		return 0, fmt.Errorf("resolve project for session %d: %w", rec.ID, err)
	}

	sess, err := s.createOrResumeSession(ctx, rec.ID, workDir, rec, false)
	if err != nil {
		return 0, fmt.Errorf("open session %d to close interrupted calls: %w", rec.ID, err)
	}
	defer sess.Close()

	if err := sess.ResolveInterruptedCalls(ctx, pending, interruptedCallNotice); err != nil {
		return 0, fmt.Errorf("close interrupted calls in session %d: %w", rec.ID, err)
	}

	return len(pending), nil
}

// storedInterruptedCalls is a session's pending in-loop calls read from the
// durable transcript — everything the loop would otherwise re-execute.
func (s *svc) storedInterruptedCalls(ctx context.Context, sessionID int64) ([]session.PendingToolCall, error) {
	stored, err := s.sessionStore.LoadActiveMessages(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load transcript of session %d: %w", sessionID, err)
	}

	return unresolvedStoredCalls(stored, func(name string) bool {
		return !tool.IsExternalCall(name)
	})
}
