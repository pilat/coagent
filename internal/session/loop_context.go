package session

import (
	"context"
	"errors"

	"go.uber.org/zap"
)

const compactionNotConvergingNotice = "⚠️ Context window too small for this workload — compaction is no " +
	"longer freeing enough space. Automatic compaction is paused for this run; switch to a model with a " +
	"larger context window."

// applyContextEvents is the single sanctioned compaction point: an explicit
// request forces, otherwise the projected request size decides.
func (r *loopRunner) applyContextEvents(ctx context.Context) {
	// The one place that decides compaction is safe: a queued request keeps its
	// place rather than being consumed into a failure.
	if r.agent.HasPendingExternalCall() || r.agent.HasPendingWork() {
		return
	}

	keepRecent := compactionKeepRecent
	explicit := false

	if pending := r.agent.consumePendingCompaction(); pending != nil {
		keepRecent = *pending
		explicit = true
	}

	window := r.agent.contextWindow()

	if !explicit && (r.autoCompactionOff || !r.agent.shouldCompact(window)) {
		return
	}

	r.notify(ctx, "🔄 Compacting context...")

	ok, err := r.agent.compact(ctx, keepRecent)

	// The focus is one-shot: it described this request, not the next one.
	r.agent.setCompactionFocus("")

	switch {
	case errors.Is(err, errCompactionHeaderTooLarge):
		r.log.Warn("compaction_header_over_threshold")
		r.notify(ctx, compactionHeaderTooLargeNotice)
	case err != nil:
		r.log.Warn("compaction_failed", zap.Error(err))
		r.notify(ctx, "❌ Compaction failed")
	case ok:
		r.notify(ctx, "✅ Context compacted")
	case explicit:
		r.notify(ctx, "Nothing to compact")
	}

	// An explicit request neither counts against the cap nor clears it.
	if !explicit {
		r.recordAutoCompaction(ctx, ok && err == nil && !r.agent.shouldCompact(window))
	}
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
	r.notify(ctx, compactionNotConvergingNotice)
}
