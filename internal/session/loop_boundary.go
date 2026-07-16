package session

import (
	"context"
	"fmt"
	"strings"

	"github.com/pilat/coagent/internal/tool"
)

const (
	compactCommand = "/compact"

	sleepInterruptedMessage = "Sleep interrupted — user sent a message."

	compactionDeferredNotice = "⏳ Compaction deferred until the session finishes waiting"
)

// boundaryOutcome tells drainBoundary what a control command did with the input.
type boundaryOutcome uint8

const (
	// commandNotRecognized: ordinary input, hand it to the normal path.
	commandNotRecognized boundaryOutcome = iota
	// commandConsumed: handled and taken off the queue; keep draining.
	commandConsumed
	// commandDeferred: left in the durable queue behind a pending call; stop draining.
	commandDeferred
)

func (r *loopRunner) drainBoundary(ctx context.Context) (bool, error) {
	if r.agent.boundary == nil {
		return false, nil
	}

	acceptedAny := false

	for {
		input, err := r.agent.boundary.Peek(ctx)
		if err != nil {
			return acceptedAny, fmt.Errorf("peek durable input: %w", err)
		}

		if input == nil {
			return acceptedAny, nil
		}

		outcome, err := r.handleBoundaryCommand(ctx, *input)
		if err != nil {
			return acceptedAny, err
		}

		switch outcome {
		case commandConsumed:
			continue
		case commandDeferred:
			// The input stays durable, so peeking it again would spin.
			return acceptedAny, nil
		case commandNotRecognized:
		}

		prepared, err := r.agent.PrepareUserMessage(input.Content)
		if err != nil {
			if rejectErr := r.agent.boundary.Reject(ctx, *input, err.Error()); rejectErr != nil {
				return acceptedAny, fmt.Errorf("reject durable input: %w", rejectErr)
			}

			if r.nothingToAnswer() {
				r.handledControl = true
			}

			r.notify(ctx, "⚠️ "+err.Error())

			continue
		}

		prepared = r.agent.stamper.stampAt(prepared, input.ReceivedAt)
		pendingCalls := r.agent.PendingExternalCalls()

		if onlySleepCalls(pendingCalls) {
			if err := r.interruptSleeps(ctx, pendingCalls); err != nil {
				return acceptedAny, fmt.Errorf("interrupt sleep for durable input: %w", err)
			}
		}

		accepted, blocked, err := r.agent.boundary.Accept(
			ctx,
			*input,
			prepared,
			pendingCalls,
		)
		if err != nil {
			return acceptedAny, fmt.Errorf("accept durable input: %w", err)
		}

		if blocked {
			return acceptedAny, nil
		}

		if !accepted {
			continue
		}

		if err := r.agent.ms.reloadMessages(ctx); err != nil {
			return acceptedAny, fmt.Errorf("reload durable input: %w", err)
		}

		acceptedAny = true

		r.agent.loopDetector.resetWindow()
	}
}

func (r *loopRunner) handleBoundaryCommand(ctx context.Context, input PendingInput) (boundaryOutcome, error) {
	trimmed := strings.TrimSpace(input.Content)

	switch {
	case trimmed == "/status":
		if err := r.agent.boundary.Handle(ctx, input, "status command"); err != nil {
			return commandNotRecognized, fmt.Errorf("resolve status command: %w", err)
		}

		if r.nothingToAnswer() {
			r.handledControl = true
		}

		r.notify(ctx, renderStatus(r.agent.buildSessionStatus(ctx)))

		return commandConsumed, nil
	case trimmed == compactCommand || strings.HasPrefix(trimmed, compactCommand+" "):
		return r.handleCompactCommand(ctx, input, trimmed)
	default:
		return commandNotRecognized, nil
	}
}

// handleCompactCommand raises the flag compact_context raises, so /compact runs
// at the loop's one sanctioned point instead of reaching in from the side.
func (r *loopRunner) handleCompactCommand(
	ctx context.Context,
	input PendingInput,
	trimmed string,
) (boundaryOutcome, error) {
	pending := r.agent.PendingExternalCalls()

	// Sleep yields to user input, /compact included: queueing it would mute the
	// ordinary messages behind it and spin the runner.
	if onlySleepCalls(pending) {
		if err := r.interruptSleeps(ctx, pending); err != nil {
			return commandNotRecognized, fmt.Errorf("interrupt sleep for compact command: %w", err)
		}

		pending = nil
	}

	// Behind anything else the request stays in the durable inbox: an in-memory
	// flag would die with the svc that the resume rebuilds.
	if len(pending) > 0 {
		if !r.agent.compactionDeferAnnounced {
			r.agent.compactionDeferAnnounced = true
			r.notify(ctx, compactionDeferredNotice)
		}

		return commandDeferred, nil
	}

	if err := r.agent.boundary.Handle(ctx, input, "compact command"); err != nil {
		return commandNotRecognized, fmt.Errorf("resolve compact command: %w", err)
	}

	if r.nothingToAnswer() {
		r.handledControl = true
	}

	r.agent.setCompactionFocus(strings.TrimSpace(strings.TrimPrefix(trimmed, compactCommand)))
	r.agent.RequestCompaction(compactionKeepRecent)

	return commandConsumed, nil
}

// nothingToAnswer reports an empty transcript (or a lone AGENTS.md header): the
// only state in which a boundary command may end the activation by itself.
func (r *loopRunner) nothingToAnswer() bool {
	msgs := r.agent.ms.getMessages()

	return len(msgs) == 0 ||
		(len(msgs) == 1 && strings.HasPrefix(msgs[0].Content, agentsMDMessagePrefix))
}

func (r *loopRunner) interruptSleeps(ctx context.Context, calls []PendingToolCall) error {
	for _, call := range calls {
		if _, err := r.agent.ResolvePendingCall(ctx, call, sleepInterruptedMessage); err != nil {
			return fmt.Errorf("resolve sleep call %s: %w", call.ID, err)
		}
	}

	return nil
}

func onlySleepCalls(calls []PendingToolCall) bool {
	if len(calls) == 0 {
		return false
	}

	for _, call := range calls {
		if call.Name != tool.IDSleep {
			return false
		}
	}

	return true
}
