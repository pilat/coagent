package session

import (
	"context"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/sessionstore"
)

const compactionNotConvergingNotice = "⚠️ Context window too small for this workload — compaction is no " +
	"longer freeing enough space. Automatic compaction is paused for this run; switch to a model with a " +
	"larger context window."

// applyContextEvents is the single sanctioned compaction point: an explicit
// request forces, otherwise the projected request size decides.
//
//nolint:gocyclo,nestif,funlen // Explicit compaction has a durable start, terminal outcome, and auto-path fallback.
func (s *Session) compactionStep(ctx context.Context, r *runState) error {
	if s.HasPendingExternalCall() || len(s.pendingInLoopCalls()) > 0 {
		return nil
	}

	command := s.compactionCommandInput()
	explicit := s.consumePendingCompaction()

	window := s.contextWindow()
	if !explicit && (r.autoCompactionOff || !s.shouldCompact(window) || !s.hasCompactionCandidate(window)) {
		return nil
	}

	fired, err := s.observeBudget(ctx)
	if err != nil {
		return err
	}

	if fired {
		s.budgetFired = true
		if command != nil {
			return s.finishCompactionCommand(
				ctx,
				*command,
				"parked",
				"⏸ Budget checkpoint reached — the session is parked. Send a message to resume.",
			)
		}

		return nil
	}

	c := s.newCommit()

	c.Outputs = []sessionstore.Output{
		{Type: sessionstore.OutputMessageReplaceable, Content: "🔄 Compacting context...", MessageRef: -1},
	}
	if command != nil {
		c.Outputs[0].Key = fmt.Sprintf("input:%d:compact:started", command.ID)
	}

	if _, err := s.commit(ctx, c); err != nil {
		return err
	}

	ok, compactErr := s.compact(ctx, command)
	s.emitCommitted(s.compactionOutputs, s.budgetFired)
	s.compactionOutputs = nil
	s.setCompactionFocus("")

	terminal := ""

	switch {
	case errors.Is(compactErr, errCompactionHeaderTooLarge):
		terminal = compactionHeaderTooLargeNotice
	case compactErr != nil:
		terminal = "❌ Compaction failed"
	case ok:
		terminal = "✅ Context compacted"
	case explicit:
		terminal = "Nothing to compact"
	}

	if command != nil {
		if !ok {
			if err := s.finishCompactionCommand(
				ctx,
				*command,
				compactionOutcomePhase(ok, compactErr),
				terminal,
			); err != nil {
				return err
			}
		}

		s.clearCompactionCommandInput()
	} else if terminal != "" {
		c := s.newCommit()

		c.Outputs = []sessionstore.Output{
			{Type: sessionstore.OutputMessagePersistent, Content: terminal, MessageRef: -1},
		}
		if ok {
			c.Outputs[0].Key = fmt.Sprintf("compaction:%d:succeeded", s.compactionSummaryDBID)
		}

		if _, err := s.commit(ctx, c); err != nil {
			return err
		}
	}

	if !explicit {
		if ok && compactErr == nil && !s.shouldCompact(window) {
			r.compactionFailures = 0
		} else {
			r.compactionFailures++
		}

		if r.compactionFailures >= compactionAttemptCap {
			r.autoCompactionOff = true
			c := s.newCommit()

			c.Outputs = []sessionstore.Output{
				{Type: sessionstore.OutputMessagePersistent, Content: compactionNotConvergingNotice, MessageRef: -1},
			}
			if _, err := s.commit(ctx, c); err != nil {
				return err
			}
		}
	}

	return nil
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

func (s *Session) finishCompactionCommand(ctx context.Context, input PendingInput, phase, content string) error {
	c := s.newCommit()
	c.Accept = []sessionstore.Accept{
		{InputID: input.ID, State: sessionstore.InputStateHandled, Reason: "compact command", LinkRef: -1},
	}
	c.Outputs = []sessionstore.Output{
		{
			Type:          sessionstore.OutputMessagePersistent,
			Content:       content,
			Key:           fmt.Sprintf("input:%d:compact:%s", input.ID, phase),
			MessageRef:    -1,
			ReleasesInput: true,
		},
	}
	_, err := s.commit(ctx, c)

	return err
}
