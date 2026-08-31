package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/sessionstore"
)

const compactionNotConvergingNotice = "⚠️ Context window too small for this workload — compaction is no " +
	"longer freeing enough space. Automatic compaction is paused for this run; switch to a model with a " +
	"larger context window."

// applyContextEvents is the single sanctioned compaction point: an explicit
// request forces, otherwise the projected request size decides.
//
//nolint:gocyclo,nestif,funlen // Explicit compaction has a durable start, terminal outcome, and auto-path fallback.
func (r *loopRunner) applyContextEvents(ctx context.Context) {
	// The one place that decides compaction is safe: a queued request keeps its
	// place rather than being consumed into a failure.
	if r.agent.HasPendingExternalCall() || r.agent.HasPendingWork() {
		return
	}

	explicit := false
	commandInput := r.agent.compactionCommandInput()

	if r.agent.consumePendingCompaction() {
		explicit = true
	}

	window := r.agent.contextWindow()

	if !explicit && (r.autoCompactionOff || !r.agent.shouldCompact(window)) {
		return
	}

	// The verbatim tail is never empty (D3): when the raw range cannot yield a
	// split, an automatic attempt would announce itself and then refuse
	// silently. The transcript keeps growing, so the next crossing gets a real
	// attempt; an explicit /compact still reports "Nothing to compact".
	if !explicit && !r.agent.hasCompactionCandidate(window) {
		return
	}

	// A fired budget parks the tree: /compact is read-only and answers with the
	// parked explanation, never with a failure claim (and spends no model call).
	if r.agent.budgetGate != nil {
		if err := r.agent.budgetGate.Admit(ctx, time.Now().UTC()); errors.Is(err, ErrBudgetCheckpoint) {
			r.finishParkedCompaction(ctx, commandInput)

			return
		}
	}

	if commandInput != nil {
		if err := r.enqueueCompactionNotice(
			ctx,
			*commandInput,
			"started",
			sessionstore.OutputMessageReplaceable,
			"🔄 Compacting context...",
		); err != nil {
			r.log.Warn("start_compaction_output_failed", zap.Error(err))
			return
		}

		r.notify(ctx, "🔄 Compacting context...")
	} else {
		r.notifyPersistent(ctx, "🔄 Compacting context...")
	}

	durableCommand := commandInput
	if _, ok := r.agent.store.(sessionstore.CompactionCommandStore); !ok || !r.agent.outputEnabled {
		durableCommand = nil
	}

	ok, err := r.agent.compact(ctx, durableCommand)

	// The focus is one-shot: it described this request, not the next one.
	r.agent.setCompactionFocus("")

	terminal := ""

	switch {
	case errors.Is(err, errCompactionHeaderTooLarge):
		r.log.Warn("compaction_header_over_threshold")

		terminal = compactionHeaderTooLargeNotice
	case err != nil:
		r.log.Warn("compaction_failed", zap.Error(err))

		terminal = "❌ Compaction failed"
	case ok:
		terminal = "✅ Context compacted"
	case explicit:
		terminal = "Nothing to compact"
	}

	if terminal != "" {
		if commandInput != nil && (!ok || durableCommand == nil) {
			phase := compactionOutcomePhase(ok, err)
			if finishErr := r.finishCompactionCommand(ctx, *commandInput, phase, terminal); finishErr != nil {
				r.log.Warn("finish_compaction_command_failed", zap.Error(finishErr))
				return
			}
		}

		if commandInput != nil {
			r.agent.clearCompactionCommandInput()
			r.notify(ctx, terminal)
		} else if ok && err == nil {
			r.notifyAutoCompactionOutcome(ctx, terminal)
		} else {
			r.notifyPersistent(ctx, terminal)
		}
	}

	// An explicit request neither counts against the cap nor clears it.
	if !explicit {
		r.recordAutoCompaction(ctx, ok && err == nil && !r.agent.shouldCompact(window))
	}
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
func (r *loopRunner) finishParkedCompaction(ctx context.Context, commandInput *PendingInput) {
	const parkedNotice = "⏸ Budget checkpoint reached — the session is parked. Send a message to resume."

	if commandInput != nil {
		if err := r.finishCompactionCommand(ctx, *commandInput, "parked", parkedNotice); err != nil {
			r.log.Warn("finish_compaction_command_failed", zap.Error(err))

			return
		}

		r.agent.clearCompactionCommandInput()
		r.notify(ctx, parkedNotice)

		return
	}

	r.notifyPersistent(ctx, parkedNotice)
}

// notifyAutoCompactionOutcome keys the success row to its summary message, so a
// crash between the summary commit and this enqueue replays as an idempotent no-op.
func (r *loopRunner) notifyAutoCompactionOutcome(ctx context.Context, content string) {
	outputs, ok := r.agent.store.(sessionstore.OutputStore)
	if !ok || !r.agent.outputEnabled || r.agent.compactionSummaryDBID == 0 {
		r.notifyPersistent(ctx, content)
		return
	}

	_, err := outputs.EnqueueOutput(ctx, sessionstore.OutputDraft{
		SessionID:   r.agent.id,
		Type:        sessionstore.OutputMessagePersistent,
		Content:     content,
		SourceKey:   fmt.Sprintf("compaction:%d:succeeded", r.agent.compactionSummaryDBID),
		Fingerprint: sessionstore.OutputFingerprint(sessionstore.OutputMessagePersistent, content, r.agent.id, nil),
	})
	if err != nil {
		r.log.Warn("enqueue_auto_compaction_output_failed", zap.Error(err))
	}

	r.notify(ctx, content)
}

func (r *loopRunner) enqueueCompactionNotice(
	ctx context.Context,
	input PendingInput,
	phase string,
	kind sessionstore.OutputType,
	content string,
) error {
	outputs, ok := r.agent.store.(sessionstore.OutputStore)
	if !ok || !r.agent.outputEnabled {
		return nil
	}

	_, err := outputs.EnqueueOutput(ctx, sessionstore.OutputDraft{
		SessionID:   r.agent.id,
		Type:        kind,
		Content:     content,
		SourceKey:   fmt.Sprintf("input:%d:compact:%s", input.ID, phase),
		Fingerprint: sessionstore.OutputFingerprint(kind, content, r.agent.id, nil),
	})
	if err != nil {
		return fmt.Errorf("enqueue compact %s: %w", phase, err)
	}

	return nil
}

func (r *loopRunner) finishCompactionCommand(
	ctx context.Context,
	input PendingInput,
	phase, content string,
) error {
	outputs, ok := r.agent.store.(sessionstore.CommandOutputStore)
	if ok && r.agent.outputEnabled {
		_, err := outputs.HandleInputWithOutput(ctx, input.ID, "compact command", sessionstore.OutputDraft{
			SessionID:   r.agent.id,
			Type:        sessionstore.OutputMessagePersistent,
			Content:     content,
			SourceKey:   fmt.Sprintf("input:%d:compact:%s", input.ID, phase),
			Fingerprint: sessionstore.OutputFingerprint(sessionstore.OutputMessagePersistent, content, r.agent.id, nil),
		})
		if err != nil {
			return fmt.Errorf("complete compact command: %w", err)
		}

		return nil
	}

	return r.handleCommandOutput(ctx, input, "compact command", content)
}

// recordAutoCompaction silences the automatic path after compactionAttemptCap
// consecutive attempts that left the projection above the threshold.
func (r *loopRunner) recordAutoCompaction(ctx context.Context, relieved bool) {
	if relieved {
		r.compactionFailures = 0
		return
	}

	r.compactionFailures++
	if r.compactionFailures < compactionAttemptCap {
		return
	}

	r.autoCompactionOff = true
	r.notifyPersistent(ctx, compactionNotConvergingNotice)
}
