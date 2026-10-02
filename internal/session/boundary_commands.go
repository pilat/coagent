package session

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/transcript"
)

func (s *svc) acceptBoundaryCommand(ctx context.Context, r *runState, batch *boundaryBatch, input *sessionstore.InboxInput, command string) error {
	state, err := s.store.LoadCompletionCheckState(ctx, s.id)
	if err != nil {
		return err
	}
	if batch.fresh || durableCandidateID(state) != 0 {
		r.handledControl = true
	}
	output := s.boundaryCommandContent(ctx, input, command)
	c := &batch.commit
	c.Accept = append(c.Accept, sessionstore.Accept{InputID: input.ID, State: sessionstore.InputStateHandled, Reason: strings.TrimPrefix(command, "/") + " command", LinkRef: -1})
	c.Outputs = append(c.Outputs, sessionstore.Output{Type: sessionstore.OutputMessagePersistent, Content: output, Key: fmt.Sprintf("input:%d:command", input.ID), MessageRef: -1, ReleasesInput: true})
	return nil
}

func (s *svc) boundaryCommandContent(ctx context.Context, input *sessionstore.InboxInput, command string) string {
	output := s.renderSessionHelp()
	switch command {
	case "/schedules":
		output = s.schedules
		if snapshot, ok := input.Attributes["schedules"].(string); ok {
			output = snapshot
		}
	case "/status":
		output = renderStatus(s.buildSessionStatus(ctx))
		if snapshot, ok := input.Attributes["status"].(string); ok {
			output = snapshot
		}
	}
	return output
}

func (s *svc) deferBoundaryCompaction(batch *boundaryBatch, input *sessionstore.InboxInput) {
	if s.compactionDeferAnnounced {
		return
	}
	batch.commit.Outputs = append(batch.commit.Outputs, sessionstore.Output{
		Type: sessionstore.OutputMessagePersistent, Content: compactionDeferredNotice,
		Key: fmt.Sprintf("input:%d:compact:deferred", input.ID), MessageRef: -1,
	})
	s.compactionDeferAnnounced = true
}

func (s *svc) interruptBoundarySleeps(batch *boundaryBatch, source sessionstore.InputSource) {
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

func (s *svc) requestBoundaryCompaction(input *sessionstore.InboxInput, content string) {
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
	return fmt.Sprintf("\n\n[Host activation: call %s as the only tool in this assistant response to handle %s. No change has occurred until the host emits its receipt.]", toolID, command)
}
