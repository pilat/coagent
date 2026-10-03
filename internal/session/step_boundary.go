package session

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

const (
	compactCommand           = "/compact"
	sleepInterruptedMessage  = "Sleep interrupted — user sent a message."
	compactionDeferredNotice = "⏳ Compaction deferred until the session finishes waiting"
)

const (
	boundaryContinue boundaryFlow = iota
	boundaryStop
)

type boundaryFlow uint8

type PendingInput struct {
	ID           int64
	Content      string
	Attributes   map[string]any
	ReceivedAt   time.Time
	ManagerOwned bool
	Source       sessionstore.InputSource
}

type boundaryBatch struct {
	commit   sessionstore.Commit
	pending  map[string]string
	fresh    bool
	accepted bool
}

func (s *Session) boundaryStep(ctx context.Context, r *runState) (bool, error) {
	for attempt := 0; attempt < 2; attempt++ {
		previousRun, previousStamp := *r, s.stamper
		previousStopped, previousDetector := s.preserveStopped, *s.loopDetector
		previousFocus, previousInput := s.compactionFocus, s.compactionInput
		previousRequested, previousAnnounced := s.pendingCompaction, s.compactionDeferAnnounced
		accepted, err := s.boundaryAttempt(ctx, r)
		if !errors.Is(err, sessionstore.ErrInputResolved) {
			return accepted, err
		}
		// A host command can win after ListPending; its rollback accepted no input.
		*r, s.stamper = previousRun, previousStamp
		s.preserveStopped, *s.loopDetector = previousStopped, previousDetector
		s.compactionFocus, s.compactionInput = previousFocus, previousInput
		s.pendingCompaction, s.compactionDeferAnnounced = previousRequested, previousAnnounced
		if reloadErr := s.ms.reloadMessages(ctx); reloadErr != nil {
			return false, reloadErr
		}
		if attempt == 1 {
			return false, err
		}
	}
	return false, sessionstore.ErrInputResolved
}

func (s *Session) boundaryAttempt(ctx context.Context, r *runState) (bool, error) {
	r.handledControl = false
	inputs, err := s.store.ListPending(ctx, s.id)
	if err != nil {
		return false, err
	}
	batch := s.newBoundaryBatch()
	acceptBoundaryCallResults(batch, inputs)
	if err := s.drainBoundaryInputs(ctx, r, batch, inputs); err != nil {
		return false, err
	}
	return s.commitBoundaryBatch(ctx, r, batch, len(inputs))
}

func (s *Session) newBoundaryBatch() *boundaryBatch {
	batch := &boundaryBatch{
		commit: s.newCommit(), pending: make(map[string]string),
		fresh: len(s.ms.getMessages()) == 0,
	}
	for _, call := range s.PendingExternalCalls() {
		batch.pending[call.ID] = call.Name
	}
	if batch.fresh && s.agentsMD != "" {
		batch.commit.Messages = append(batch.commit.Messages, hostUserMessage(agentsMDMessagePrefix+s.agentsMD))
	}
	return batch
}

func (s *Session) drainBoundaryInputs(
	ctx context.Context,
	r *runState,
	batch *boundaryBatch,
	inputs []*sessionstore.InboxInput,
) error {
	for _, input := range inputs {
		if input.Source == sessionstore.InputSourceCallResult {
			continue
		}
		flow, err := s.handleBoundaryInput(ctx, r, batch, input)
		if err != nil {
			return err
		}
		if flow == boundaryStop {
			break
		}
	}
	return nil
}

func (s *Session) handleBoundaryInput(
	ctx context.Context,
	r *runState,
	batch *boundaryBatch,
	input *sessionstore.InboxInput,
) (boundaryFlow, error) {
	trimmed := strings.TrimSpace(input.RawContent)
	command := leadingSlashCommand(input.RawContent)
	sleepsOnly := pendingSleepsOnly(batch.pending)
	if s.boundaryInputBlocked(batch, input, trimmed, command, sleepsOnly) {
		return boundaryStop, nil
	}
	if s.preserveStopped &&
		(input.Source == sessionstore.InputSourceProcess || input.Source == sessionstore.InputSourceSubagent) {
		return boundaryContinue, nil
	}
	if trimmed == "/help" || trimmed == "/status" || trimmed == "/schedules" {
		return boundaryContinue, s.acceptBoundaryCommand(ctx, r, batch, input, trimmed)
	}
	isCompact := trimmed == compactCommand || strings.HasPrefix(trimmed, compactCommand+" ")
	if !isCompact && len(s.pendingInLoopCalls()) > 0 {
		return boundaryStop, nil
	}
	if isCompact && len(batch.pending) > 0 && !sleepsOnly {
		s.deferBoundaryCompaction(batch, input)
		return boundaryStop, nil
	}
	if len(batch.pending) > 0 && sleepsOnly {
		s.interruptBoundarySleeps(batch, input.Source)
	}
	if isCompact {
		s.requestBoundaryCompaction(input, trimmed)
		return boundaryStop, nil
	}
	if input.Source == sessionstore.InputSourceSchedule {
		return s.acceptBoundarySchedule(batch, input), nil
	}
	return s.acceptBoundaryText(ctx, r, batch, input, command)
}

func (s *Session) boundaryInputBlocked(
	batch *boundaryBatch,
	input *sessionstore.InboxInput,
	trimmed, command string,
	sleepsOnly bool,
) bool {
	if input.Source == sessionstore.InputSourceUser &&
		(trimmed == "/stop" || trimmed == "/clear" || trimmed == "/kill") {
		return true
	}
	if s.currentActivation != nil && input.ID != s.currentActivation.InputID {
		return true
	}
	if batch.accepted && command != "" {
		return true
	}
	return command == "" && (len(s.pendingInLoopCalls()) > 0 || (len(batch.pending) > 0 && !sleepsOnly))
}

func pendingSleepsOnly(pending map[string]string) bool {
	for _, name := range pending {
		if name != tool.IDSleep {
			return false
		}
	}
	return true
}

func (s *Session) commitBoundaryBatch(
	ctx context.Context,
	r *runState,
	batch *boundaryBatch,
	inputCount int,
) (bool, error) {
	c := &batch.commit
	if batch.fresh && !batch.accepted && inputCount == 0 {
		c.Messages = append(c.Messages, hostUserMessage(s.stamper.Stamp(noTaskPrompt)))
		batch.accepted = true
	}
	if batch.fresh && !batch.accepted {
		c.Messages = nil
	}
	willModel := batch.accepted || s.unansweredWork() || len(c.ToolResults) > 0
	if willModel && len(batch.pending) == 0 && !r.backgroundInserted && s.activeBackgroundSnapshot != "" {
		c.Messages = append(c.Messages, hostUserMessage(s.activeBackgroundSnapshot))
		r.backgroundInserted = true
	}
	if len(c.Accept) == 0 && len(c.Messages) == 0 && len(c.ToolResults) == 0 && len(c.Outputs) == 0 {
		return batch.accepted, nil
	}
	result, err := s.commit(ctx, *c)
	if err != nil {
		return false, err
	}
	if c.State.ResetContext {
		s.prompt.Todos.Clear()
		s.resetContextBaseline()
		s.loopDetector.resetWindow()
	}
	if result.Activation != nil {
		grant := result.Activation
		s.currentActivation = &tool.ActivationGrant{
			SessionID: grant.SessionID,
			InputID:   grant.InputID,
			ToolID:    grant.ToolID,
			Command:   grant.Command,
		}
	}
	return batch.accepted, nil
}
