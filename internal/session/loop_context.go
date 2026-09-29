package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionstore"
)

const compactionNotConvergingNotice = "⚠️ Context window too small for this workload — compaction is no " +
	"longer freeing enough space. Automatic compaction is paused for this run; switch to a model with a " +
	"larger context window."

func (r *loopRunner) applyContextEvents(ctx context.Context) {
	if r.agent.HasPendingExternalCall() || r.agent.HasPendingWork() {
		return
	}

	result := r.agent.contexts.apply(ctx, r.opts.Notify)
	if result.budgetFired {
		r.agent.budgetFired = true
	}
}

//nolint:gocyclo,nestif,funlen // Explicit compaction has a durable start, terminal outcome, and auto-path fallback.
func (s *checkpointOwner) apply(ctx context.Context, notify func(context.Context, string) error) checkpointResult {
	log := logger.Ctx(ctx).Named("session.compaction")
	s.notifyFn = notify
	outcome := checkpointResult{}
	// The one place that decides compaction is safe: a queued request keeps its
	// place rather than being consumed into a failure.
	if len(s.calls.PendingExternalCalls()) > 0 || s.calls.HasPendingWork() {
		return outcome
	}

	attempt := s.control.claim()
	explicit := attempt != nil

	var commandInput *PendingInput
	if attempt != nil {
		commandInput = attempt.input
		if attempt.terminal != nil {
			if err := s.finishCompactionCommand(ctx, *commandInput,
				attempt.terminal.phase, attempt.terminal.content); err != nil {
				log.Warn("finish_compaction_command_failed", zap.Error(err))
				return attempt.terminal.outcome
			}

			s.control.finish(attempt)
			s.notify(ctx, attempt.terminal.content)

			return attempt.terminal.outcome
		}
	}

	window := s.models.snapshot().contextWindow

	if !explicit && (s.autoCompactionOff || !s.shouldCompact(window)) {
		return outcome
	}

	// The verbatim tail is never empty (D3): when the raw range cannot yield a
	// split, an automatic attempt would announce itself and then refuse
	// silently. The transcript keeps growing, so the next crossing gets a real
	// attempt; an explicit /compact still reports "Nothing to compact".
	if !explicit && !s.hasCompactionCandidate(window) {
		return outcome
	}

	// A fired budget parks the tree: /compact is read-only and answers with the
	// parked explanation, never with a failure claim (and spends no model call).
	if s.budgetGate != nil {
		if err := s.budgetGate.Admit(ctx, time.Now().UTC()); errors.Is(err, ErrBudgetCheckpoint) {
			if s.finishParkedCompaction(ctx, commandInput) {
				s.control.finish(attempt)
			}

			outcome.budgetFired = true

			return outcome
		}
	}

	if commandInput != nil {
		if err := s.enqueueCompactionNotice(
			ctx,
			*commandInput,
			"started",
			sessionstore.OutputMessageReplaceable,
			"🔄 Compacting context...",
		); err != nil {
			log.Warn("start_compaction_output_failed", zap.Error(err))
			return outcome
		}

		s.notify(ctx, "🔄 Compacting context...")
	} else {
		s.notifyPersistent(ctx, "🔄 Compacting context...")
	}

	durableCommand := commandInput
	if s.outputStore == nil || !s.outputEnabled {
		durableCommand = nil
	}

	outcome, err := s.checkpoint(ctx, durableCommand)
	ok := outcome.committed

	terminal := ""

	switch {
	case errors.Is(err, errCompactionHeaderTooLarge):
		log.Warn("compaction_header_over_threshold")

		terminal = compactionHeaderTooLargeNotice
	case err != nil:
		log.Warn("compaction_failed", zap.Error(err))

		terminal = "❌ Compaction failed"
	case ok:
		terminal = "✅ Context compacted"
	case explicit:
		terminal = "Nothing to compact"
	}

	if terminal != "" {
		if commandInput != nil && (!ok || durableCommand == nil) {
			phase := compactionOutcomePhase(ok, err)

			attempt.terminal = &checkpointTerminal{
				phase: phase, content: terminal, outcome: outcome,
			}
			if finishErr := s.finishCompactionCommand(ctx, *commandInput, phase, terminal); finishErr != nil {
				log.Warn("finish_compaction_command_failed", zap.Error(finishErr))
				return outcome
			}
		}

		if commandInput != nil {
			s.control.finish(attempt)
			s.notify(ctx, terminal)
		} else if ok && err == nil {
			s.notifyAutoCompactionOutcome(ctx, terminal)
		} else {
			s.notifyPersistent(ctx, terminal)
		}
	}

	s.control.finish(attempt)

	// An explicit request neither counts against the cap nor clears it.
	if !explicit {
		s.recordAutoCompaction(ctx, ok && err == nil && !s.shouldCompact(window))
	}

	return outcome
}

func compactionOutcomePhase(ok bool, err error) string {
	if ok {
		return "succeeded"
	}

	if err == nil {
		return "nothing"
	}

	return "failed"
}

// finishParkedCompaction resolves a compaction request against a fired budget:
// the command gets its durable outcome, the root stays parked.
func (s *checkpointOwner) finishParkedCompaction(ctx context.Context, commandInput *PendingInput) bool {
	log := logger.Ctx(ctx).Named("session.compaction")

	const parkedNotice = "⏸ Budget checkpoint reached — the session is parked. Send a message to resume."
	if commandInput != nil {
		if err := s.finishCompactionCommand(ctx, *commandInput, "parked", parkedNotice); err != nil {
			log.Warn("finish_compaction_command_failed", zap.Error(err))
			return false
		}

		s.notify(ctx, parkedNotice)

		return true
	}

	s.notifyPersistent(ctx, parkedNotice)

	return true
}

// notifyAutoCompactionOutcome keys the success row to its summary message, so a
// crash between the summary commit and this enqueue replays as an idempotent no-op.
func (s *checkpointOwner) notifyAutoCompactionOutcome(ctx context.Context, content string) {
	log := logger.Ctx(ctx).Named("session.compaction")
	if s.outputStore == nil || !s.outputEnabled || s.compactionSummaryDBID == 0 {
		s.notifyPersistent(ctx, content)
		return
	}

	_, err := s.outputStore.EnqueueOutput(ctx, sessionstore.OutputDraft{
		SessionID:   s.id,
		Type:        sessionstore.OutputMessagePersistent,
		Content:     content,
		SourceKey:   fmt.Sprintf("compaction:%d:succeeded", s.compactionSummaryDBID),
		Fingerprint: sessionstore.OutputFingerprint(sessionstore.OutputMessagePersistent, content, s.id, nil),
	})
	if err != nil {
		log.Warn("enqueue_auto_compaction_output_failed", zap.Error(err))
	}

	s.notify(ctx, content)
}

func (s *checkpointOwner) enqueueCompactionNotice(
	ctx context.Context,
	input PendingInput,
	phase string,
	kind sessionstore.OutputType,
	content string,
) error {
	if s.outputStore == nil || !s.outputEnabled {
		return nil
	}

	_, err := s.outputStore.EnqueueOutput(ctx, sessionstore.OutputDraft{
		SessionID:   s.id,
		Type:        kind,
		Content:     content,
		SourceKey:   fmt.Sprintf("input:%d:compact:%s", input.ID, phase),
		Fingerprint: sessionstore.OutputFingerprint(kind, content, s.id, nil),
	})
	if err != nil {
		return fmt.Errorf("enqueue compact %s: %w", phase, err)
	}

	return nil
}

func (s *checkpointOwner) finishCompactionCommand(
	ctx context.Context,
	input PendingInput,
	phase, content string,
) error {
	if s.outputStore != nil && s.outputEnabled {
		_, err := s.outputStore.HandleInputWithOutput(ctx, input.ID, "compact command", sessionstore.OutputDraft{
			SessionID:   s.id,
			Type:        sessionstore.OutputMessagePersistent,
			Content:     content,
			SourceKey:   fmt.Sprintf("input:%d:compact:%s", input.ID, phase),
			Fingerprint: sessionstore.OutputFingerprint(sessionstore.OutputMessagePersistent, content, s.id, nil),
		})
		if err != nil {
			return fmt.Errorf("complete compact command: %w", err)
		}

		return nil
	}

	return s.handleCommandOutput(ctx, input, "compact command", content)
}

// recordAutoCompaction silences the automatic path after compactionAttemptCap
// consecutive attempts that left the projection above the threshold.
func (s *checkpointOwner) recordAutoCompaction(ctx context.Context, relieved bool) {
	if relieved {
		s.compactionFailures = 0
		return
	}

	s.compactionFailures++
	if s.compactionFailures < compactionAttemptCap {
		return
	}

	s.autoCompactionOff = true
	s.notifyPersistent(ctx, compactionNotConvergingNotice)
}
