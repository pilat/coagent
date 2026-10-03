package session

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/transcript"
)

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

func (s *Session) acceptBoundarySchedule(batch *boundaryBatch, input *sessionstore.InboxInput) boundaryFlow {
	c := &batch.commit
	accept := sessionstore.Accept{
		InputID:    input.ID,
		State:      sessionstore.InputStateAccepted,
		ModelBound: true,
		LinkRef:    -1,
	}
	fresh, _ := input.Attributes["fresh"].(bool)
	if fresh && batch.accepted {
		return boundaryStop
	}
	if fresh {
		c.State.ResetContext = true
		c.Messages = nil
		for _, message := range s.openingTurn(input.RawContent) {
			c.Messages = append(c.Messages, hostUserMessage(message.Content))
		}
	} else {
		callID := fmt.Sprintf("schedule_%d", input.ID)
		calls, _ := json.Marshal([]llmwire.ToolCall{{ID: callID, Name: "schedule", Arguments: json.RawMessage("{}")}})
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
		deliveryKey = fmt.Sprintf("%d", input.ID)
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
	return boundaryContinue
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
