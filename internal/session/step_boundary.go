package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/transcript"
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
	for attempt := range 2 {
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
	r.boundaryAgain = false

	inputs, err := s.store.ListPending(ctx, s.id)
	if err != nil {
		return false, fmt.Errorf("boundary attempt: %w", err)
	}

	batch := s.newBoundaryBatch()
	acceptBoundaryCallResults(batch, inputs)

	if len(batch.commit.ToolResults) == 0 {
		if err := s.drainBoundaryInputs(ctx, r, batch, inputs); err != nil {
			return false, err
		}
	}

	// Commit writes accepted text before ToolResults, so settle calls in their own boundary step.
	r.boundaryAgain = len(batch.commit.ToolResults) > 0

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
		return boundaryStop, nil
	}

	if isCompact {
		s.requestBoundaryCompaction(input, trimmed)
		return boundaryStop, nil
	}

	if input.Source == sessionstore.InputSourceSchedule {
		return s.acceptBoundarySchedule(batch, input)
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

	willModel := batch.accepted || s.unansweredWork() || len(c.ToolResults) > 0
	if willModel && !r.boundaryAgain && len(batch.pending) == 0 && !r.backgroundInserted &&
		s.activeBackgroundSnapshot != "" {
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

func (s *Session) acceptBoundaryCommand(
	ctx context.Context,
	r *runState,
	batch *boundaryBatch,
	input *sessionstore.InboxInput,
	command string,
) error {
	state, err := s.store.LoadCompletionCheckState(ctx, s.id)
	if err != nil {
		return fmt.Errorf("accept boundary command: %w", err)
	}

	if batch.fresh || durableCandidateID(state) != 0 {
		r.handledControl = true
	}

	output := s.boundaryCommandContent(ctx, input, command)
	c := &batch.commit
	c.Accept = append(
		c.Accept,
		sessionstore.Accept{
			InputID: input.ID,
			State:   sessionstore.InputStateHandled,
			Reason:  strings.TrimPrefix(command, "/") + " command",
			LinkRef: -1,
		},
	)
	c.Outputs = append(
		c.Outputs,
		sessionstore.Output{
			Type:          sessionstore.OutputMessagePersistent,
			Content:       output,
			Key:           fmt.Sprintf("input:%d:%s:result", input.ID, strings.TrimPrefix(command, "/")),
			MessageRef:    -1,
			ReleasesInput: false,
		},
	)

	return nil
}

func (s *Session) boundaryCommandContent(ctx context.Context, input *sessionstore.InboxInput, command string) string {
	output := s.renderSessionHelp()

	switch command {
	case "/schedules":
		output = s.schedules
		if snapshot, ok := input.Attributes["schedules"].(string); ok {
			output = snapshot
		}
	case "/status":
		output = renderStatus(s.buildSessionStatus(ctx))
		if s.status != "" {
			output = s.status
		}

		if snapshot, ok := input.Attributes["status"].(string); ok {
			output = snapshot
		}
	}

	return output
}

func (s *Session) deferBoundaryCompaction(batch *boundaryBatch, input *sessionstore.InboxInput) {
	if s.compactionDeferAnnounced {
		return
	}

	batch.commit.Outputs = append(batch.commit.Outputs, sessionstore.Output{
		Type: sessionstore.OutputMessagePersistent, Content: compactionDeferredNotice,
		Key: fmt.Sprintf("input:%d:compact:deferred", input.ID), MessageRef: -1,
	})
	s.compactionDeferAnnounced = true
}

func (s *Session) interruptBoundarySleeps(batch *boundaryBatch, source sessionstore.InputSource) {
	notice := sleepInterruptedMessage
	if source == sessionstore.InputSourceSchedule {
		notice = "Sleep interrupted — a scheduled task became due."
	}

	for _, call := range s.PendingExternalCalls() {
		if batch.pending[call.ID] != "" {
			batch.commit.ToolResults = append(batch.commit.ToolResults, &transcript.Message{
				Role: llmwire.RoleTool, ToolCallID: call.ID, ToolName: call.Name, Content: notice,
			})
			delete(batch.pending, call.ID)
		}
	}
}

func (s *Session) requestBoundaryCompaction(input *sessionstore.InboxInput, content string) {
	s.setCompactionFocus(strings.TrimSpace(strings.TrimPrefix(content, compactCommand)))
	s.setCompactionCommandInput(PendingInput{ID: input.ID, Content: input.RawContent})
	s.RequestCompaction()
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

func acceptBoundaryCallResults(batch *boundaryBatch, inputs []*sessionstore.InboxInput) {
	for _, input := range inputs {
		if input.Source != sessionstore.InputSourceCallResult {
			continue
		}

		callID, _ := input.Attributes["call_id"].(string)
		toolID, _ := input.Attributes["tool_id"].(string)

		accept := sessionstore.Accept{
			InputID: input.ID,
			State:   sessionstore.InputStateRejected,
			Reason:  "stale call result",
			LinkRef: -1,
		}
		if batch.pending[callID] == toolID && toolID != "" {
			batch.commit.ToolResults = append(
				batch.commit.ToolResults,
				&transcript.Message{
					Role:       llmwire.RoleTool,
					ToolCallID: callID,
					ToolName:   toolID,
					Content:    input.RawContent,
				},
			)
			accept.State = sessionstore.InputStateHandled
			accept.Reason = "call_result"
			accept.InvalidateCompletion = true

			delete(batch.pending, callID)
		}

		batch.commit.Accept = append(batch.commit.Accept, accept)
	}
}

func (s *Session) acceptBoundarySchedule(batch *boundaryBatch, input *sessionstore.InboxInput) (boundaryFlow, error) {
	c := &batch.commit
	accept := sessionstore.Accept{
		InputID:    input.ID,
		State:      sessionstore.InputStateAccepted,
		ModelBound: true,
		LinkRef:    -1,
	}

	fresh, _ := input.Attributes["fresh"].(bool)
	if fresh && batch.accepted {
		return boundaryStop, nil
	}

	if fresh {
		c.State.ResetContext = true

		c.Messages = nil
		for _, message := range s.openingTurn(input.RawContent) {
			c.Messages = append(c.Messages, hostUserMessage(message.Content))
		}
	} else {
		callID := fmt.Sprintf("schedule_%d", input.ID)

		calls, err := json.Marshal([]llmwire.ToolCall{{ID: callID, Name: "schedule", Arguments: json.RawMessage("{}")}})
		if err != nil {
			return boundaryStop, fmt.Errorf("encode scheduled call: %w", err)
		}

		c.Messages = append(
			c.Messages,
			&transcript.Message{Role: llmwire.RoleAssistant, ToolCalls: calls},
			&transcript.Message{
				Role:       llmwire.RoleTool,
				ToolCallID: callID,
				ToolName:   "schedule",
				Content:    input.RawContent,
			},
		)
	}

	accept.LinkRef = len(c.Messages) - 1
	c.Accept = append(c.Accept, accept)

	deliveryKey := input.DeliveryKey
	if deliveryKey == "" {
		deliveryKey = strconv.FormatInt(input.ID, 10)
	}

	c.Outputs = append(
		c.Outputs,
		sessionstore.Output{
			Type:       sessionstore.OutputMessagePersistent,
			Content:    "⏰ scheduled\n\n" + input.RawContent,
			Key:        "schedule:" + deliveryKey + ":announcement",
			Attributes: map[string]any{"source": "scheduler"},
			MessageRef: -1,
		},
	)
	batch.accepted = true
	s.preserveStopped = false

	return boundaryContinue, nil
}

func (s *Session) acceptBoundaryText(
	ctx context.Context,
	r *runState,
	batch *boundaryBatch,
	input *sessionstore.InboxInput,
	command string,
) (boundaryFlow, error) {
	prepared, err := s.PrepareUserMessageDetailed(input.RawContent)
	if err != nil {
		rejectBoundaryInput(batch, input, err)

		if batch.fresh {
			r.handledControl = true
		}

		return boundaryContinue, nil
	}

	c := &batch.commit
	content := s.prompt.AppendGitStateDelta(
		ctx,
		s.stamper.StampAt(prepared.Content, input.ReceivedAt),
		s.ms.getMessages(),
	)
	accept := sessionstore.Accept{
		InputID:    input.ID,
		State:      sessionstore.InputStateAccepted,
		Content:    content,
		LinkRef:    -1,
		ModelBound: true,
	}

	owner, _ := input.Attributes["manager_id"].(string)
	if prepared.SkillName != "" && input.Source == sessionstore.InputSourceUser && owner != "" {
		accept.Receipt = loader.SkillReceipt(prepared.SkillName)
	}

	if toolID := s.activationIndex[command]; toolID != "" {
		accept.Content += activationInstruction(toolID, command)
		c.Activation = &sessionstore.ActivationChange{
			InputID: input.ID,
			State:   sessionstore.ActivationPending,
			ToolID:  toolID,
			Command: command,
		}
	}

	c.Messages = append(c.Messages, hostUserMessage(accept.Content))
	accept.Content = ""
	accept.LinkRef = len(c.Messages) - 1
	c.Accept = append(c.Accept, accept)
	batch.accepted = true

	if input.Source == sessionstore.InputSourceUser || input.Source == sessionstore.InputSourceAgent {
		s.preserveStopped = false
	}

	if owner != "" && input.Source == sessionstore.InputSourceUser {
		r.directReply = true
	}

	s.loopDetector.resetWindow()

	if command != "" || c.Activation != nil {
		return boundaryStop, nil
	}

	return boundaryContinue, nil
}

func rejectBoundaryInput(batch *boundaryBatch, input *sessionstore.InboxInput, cause error) {
	c := &batch.commit
	c.Accept = append(
		c.Accept,
		sessionstore.Accept{
			InputID: input.ID,
			State:   sessionstore.InputStateRejected,
			Reason:  cause.Error(),
			LinkRef: -1,
		},
	)
	c.Outputs = append(
		c.Outputs,
		sessionstore.Output{
			Type:          sessionstore.OutputMessagePersistent,
			Content:       "⚠️ " + cause.Error(),
			Key:           fmt.Sprintf("input:%d:rejected", input.ID),
			MessageRef:    -1,
			ReleasesInput: true,
		},
	)
}
