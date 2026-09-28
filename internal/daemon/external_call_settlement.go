package daemon

import (
	"context"
	"fmt"

	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

// SettleStopped requires every live transcript writer to have joined.
func (s *externalCalls) SettleStopped(ctx context.Context, sessionID int64) error {
	// The producer ledger is in-memory, so a stop whose second phase runs in a
	// later image owns nothing; the transcript is the only complete list.
	pending, err := s.storedExternalCalls(ctx, sessionID)
	if err != nil {
		return err
	}

	for _, call := range pending {
		s.staged.stage(sessionID, call.ID, call.Name)
	}

	sess, err := s.openTranscript(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("open stopping session %d: %w", sessionID, err)
	}

	if err := sess.SettleStoppedCalls(ctx, "Stopped by user."); err != nil {
		return fmt.Errorf("settle stopped calls for session %d: %w", sessionID, err)
	}

	for _, call := range pending {
		s.staged.resolve(sessionID, call.ID)
	}

	return nil
}

// CloseOrphans requires the startup fence before any runner can open.
func (s *externalCalls) CloseOrphans(ctx context.Context, rec *sessionstore.SessionRecord) (int, error) {
	orphans, err := s.orphanedCalls(ctx, rec.ID)
	if err != nil || len(orphans) == 0 {
		return 0, err
	}

	// Adoption precedes construction: the session refuses to resolve a call no
	// producer ledger owns, and it snapshots that ledger when it is built.
	for _, call := range orphans {
		s.staged.stage(rec.ID, call.ID, call.Name)
	}

	// A call left uninserted returns to being an orphan and is retried next boot.
	defer func() {
		for _, call := range orphans {
			s.staged.resolve(rec.ID, call.ID)
		}
	}()

	sess, err := s.openTranscript(ctx, rec.ID)
	if err != nil {
		return 0, fmt.Errorf("open session %d to close orphaned calls: %w", rec.ID, err)
	}

	for _, call := range orphans {
		if _, err := sess.ResolvePendingCall(ctx, call, orphanedCallNotice(call.Name)); err != nil {
			return 0, fmt.Errorf("close orphaned call %s in session %d: %w", call.ID, rec.ID, err)
		}
	}

	return len(orphans), nil
}

// CloseInterrupted settles in-loop calls without replaying their possibly completed effects.
func (s *externalCalls) CloseInterrupted(ctx context.Context, rec *sessionstore.SessionRecord) (int, error) {
	pending, err := s.storedInterruptedCalls(ctx, rec.ID)
	if err != nil || len(pending) == 0 {
		return 0, err
	}

	sess, err := s.openTranscript(ctx, rec.ID)
	if err != nil {
		return 0, fmt.Errorf("open session %d to close interrupted calls: %w", rec.ID, err)
	}

	if err := sess.ResolveInterruptedCalls(ctx, pending, interruptedCallNotice); err != nil {
		return 0, fmt.Errorf("close interrupted calls in session %d: %w", rec.ID, err)
	}

	return len(pending), nil
}

// storedInterruptedCalls is a session's pending in-loop calls read from the
// durable transcript — everything the loop would otherwise re-execute.
func (s *externalCalls) storedInterruptedCalls(
	ctx context.Context,
	sessionID int64,
) ([]session.PendingToolCall, error) {
	stored, err := s.sessionStore.LoadActiveMessages(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load transcript of session %d: %w", sessionID, err)
	}

	return unresolvedStoredCalls(stored, func(name string) bool {
		return !tool.IsExternalCall(name)
	})
}

func (s *externalCalls) openTranscript(ctx context.Context, sessionID int64) (session.TranscriptSession, error) {
	calls, err := s.Pending(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	transcript, err := session.OpenTranscript(ctx, s.runtimeStore, nil, sessionID, calls)
	if err != nil {
		return nil, fmt.Errorf("open session transcript: %w", err)
	}

	return transcript, nil
}
