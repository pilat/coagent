package session

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/pilat/coagent/internal/sessionprompt"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/transcript"
)

// prepareBackgroundExit nudges a subagent that stops while its own processes still run.
// It shares the one-nudge-per-generation marker with the completion check.
func (s *Session) prepareBackgroundExit(
	ctx context.Context,
	c *sessionstore.Commit,
	state *sessionstore.CompletionCheckState,
) (bool, error) {
	if s.parentID == 0 || (state != nil && state.NudgedThisGeneration) {
		return false, nil
	}

	live, err := s.store.ListLiveProcesses(ctx, s.id)
	if err != nil {
		return false, fmt.Errorf("prepare background exit: %w", err)
	}

	if len(live) == 0 {
		return false, nil
	}

	c.Unfired.State.MarkCompletionNudge = true
	c.Unfired.Messages = []*transcript.Message{
		hostUserMessage(sessionprompt.RenderBackgroundExitNudge(s.liveProcessNotes(live, time.Now().UTC()))),
	}

	return true, nil
}

func (s *Session) liveProcessNotes(live []sessionstore.LiveProcess, now time.Time) []sessionprompt.LiveProcess {
	commands := make(map[string]string, len(live))
	for _, process := range live {
		commands[process.ToolCallID] = ""
	}

	for _, message := range s.ms.getMessages() {
		for _, call := range message.ToolCalls {
			if _, wanted := commands[call.ID]; !wanted {
				continue
			}

			var args struct {
				Command string `json:"command"`
			}
			if json.Unmarshal(call.Arguments, &args) == nil {
				commands[call.ID] = args.Command
			}
		}
	}

	notes := make([]sessionprompt.LiveProcess, 0, len(live))
	for _, process := range live {
		notes = append(notes, sessionprompt.LiveProcess{
			ID: process.ID, Command: commands[process.ToolCallID], Running: max(now.Sub(process.CreatedAt), 0),
		})
	}

	return notes
}
