package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/pilat/coagent/internal/sessionstore"
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

type outputCommandBoundary interface {
	HandleWithOutput(context.Context, PendingInput, string, string) error
}

type schedulesCommandBoundary interface {
	HandleSchedules(context.Context, PendingInput) (string, error)
}

type activatedInputBoundary interface {
	AcceptActivated(
		context.Context,
		PendingInput,
		string,
		[]PendingToolCall,
		tool.ActivationGrant,
	) (bool, bool, error)
}

type activationResolver interface {
	ExpireActivation(context.Context, tool.ActivationGrant) error
}

type activationCancelBoundary interface {
	CancelActivation(context.Context, tool.ActivationGrant) error
}

type activationStateBoundary interface {
	PendingActivation(context.Context) (*tool.ActivationGrant, error)
}

type progressInputBoundary interface {
	CurrentProgress(context.Context) (string, error)
}

type progressChangeBoundary interface {
	ProgressChange(context.Context) (string, error)
}

type finalOutputBoundary interface {
	FinalOutput(context.Context, string) (string, error)
}

//nolint:funlen,gocognit,gocyclo // FIFO command, activation, and pending-call cases share one drain boundary.
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

		if r.agent.currentActivation != nil && input.ID != r.agent.currentActivation.InputID {
			return acceptedAny, nil
		}

		command := leadingSlashCommand(input.Content)
		if acceptedAny && command != "" {
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

			r.notifyPersistent(ctx, "⚠️ "+err.Error())

			continue
		}

		prepared = r.agent.stamper.stampAt(prepared, input.ReceivedAt)
		pendingCalls := r.agent.PendingExternalCalls()

		if onlySleepCalls(pendingCalls) {
			if err := r.interruptSleeps(ctx, pendingCalls); err != nil {
				return acceptedAny, fmt.Errorf("interrupt sleep for durable input: %w", err)
			}
		}

		toolID := r.agent.activationIndex[command]
		var grant *tool.ActivationGrant
		var accepted, blocked bool

		if toolID != "" {
			value := tool.ActivationGrant{
				SessionID: r.agent.id, InputID: input.ID, ToolID: toolID, Command: command,
			}
			prepared += activationInstruction(toolID, command)

			boundary, ok := r.agent.boundary.(activatedInputBoundary)
			if !ok {
				return acceptedAny, errors.New("activation boundary unavailable")
			}

			accepted, blocked, err = boundary.AcceptActivated(ctx, *input, prepared, pendingCalls, value)
			grant = &value
		} else {
			accepted, blocked, err = r.agent.boundary.Accept(ctx, *input, prepared, pendingCalls)
		}

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

		if grant != nil {
			r.agent.currentActivation = grant
			return acceptedAny, nil
		}

		if command != "" {
			return acceptedAny, nil
		}

		r.agent.loopDetector.resetWindow()
	}
}

func leadingSlashCommand(content string) string {
	trimmed := strings.TrimLeftFunc(content, unicode.IsSpace)
	if trimmed == "" || trimmed[0] != '/' {
		return ""
	}

	for i, r := range trimmed {
		if unicode.IsSpace(r) {
			return trimmed[:i]
		}
	}

	return trimmed
}

func activationInstruction(toolID, command string) string {
	return fmt.Sprintf(
		"\n\n[Host activation: call %s as the only tool in this assistant response to handle %s. No change has occurred until the host emits its receipt.]",
		toolID,
		command,
	)
}

func (r *loopRunner) handleBoundaryCommand(ctx context.Context, input PendingInput) (boundaryOutcome, error) {
	trimmed := strings.TrimSpace(input.Content)

	switch {
	case trimmed == "/status":
		if r.nothingToAnswer() {
			r.handledControl = true
		}

		output := renderStatus(r.agent.buildSessionStatus(ctx))
		if provider, ok := r.agent.boundary.(progressInputBoundary); ok {
			var err error

			output, err = provider.CurrentProgress(ctx)
			if err != nil {
				return commandNotRecognized, fmt.Errorf("capture session progress: %w", err)
			}
		}

		if err := r.handleCommandOutput(ctx, input, "status command", output); err != nil {
			return commandNotRecognized, err
		}

		r.notify(ctx, output)

		return commandConsumed, nil
	case trimmed == "/help":
		output := r.agent.renderSessionHelp()
		if err := r.handleCommandOutput(ctx, input, "help command", output); err != nil {
			return commandNotRecognized, err
		}

		r.notify(ctx, output)

		return commandConsumed, nil
	case trimmed == "/schedules":
		boundary, ok := r.agent.boundary.(schedulesCommandBoundary)
		if !ok {
			return commandNotRecognized, nil
		}

		output, err := boundary.HandleSchedules(ctx, input)
		if err != nil {
			return commandNotRecognized, fmt.Errorf("resolve schedules command: %w", err)
		}

		r.notify(ctx, output)

		return commandConsumed, nil
	case trimmed == compactCommand || strings.HasPrefix(trimmed, compactCommand+" "):
		return r.handleCompactCommand(ctx, input, trimmed)
	default:
		return commandNotRecognized, nil
	}
}

func (r *loopRunner) handleCommandOutput(ctx context.Context, input PendingInput, reason, output string) error {
	if boundary, ok := r.agent.boundary.(outputCommandBoundary); ok {
		if err := boundary.HandleWithOutput(ctx, input, reason, output); err != nil {
			return fmt.Errorf("resolve %s: %w", reason, err)
		}

		return nil
	}

	if err := r.agent.boundary.Handle(ctx, input, reason); err != nil {
		return fmt.Errorf("resolve %s: %w", reason, err)
	}

	return r.agent.enqueuePersistentOutput(ctx, output)
}

// handleCompactCommand queues the request, so /compact runs at the loop's one
// sanctioned point instead of reaching in from the side.
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
			if err := r.enqueueCompactionNotice(
				ctx,
				input,
				"deferred",
				sessionstore.OutputMessagePersistent,
				compactionDeferredNotice,
			); err != nil {
				return commandNotRecognized, err
			}

			r.notify(ctx, compactionDeferredNotice)
		}

		return commandDeferred, nil
	}

	if r.nothingToAnswer() {
		r.handledControl = true
	}

	r.agent.setCompactionFocus(strings.TrimSpace(strings.TrimPrefix(trimmed, compactCommand)))
	r.agent.setCompactionCommandInput(input)
	r.agent.RequestCompaction()

	return commandDeferred, nil
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
