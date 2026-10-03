package session

import (
	"fmt"
	"strings"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionprompt"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/transcript"
)

func prepareToolResponse(
	r *runState,
	c *sessionstore.Commit,
	response *llmwire.Response,
	state *sessionstore.CompletionCheckState,
) {
	c.State.Candidate = &sessionstore.CandidateChange{Expected: durableCandidateID(state), NextRef: -1}
	if r.directReply && strings.TrimSpace(response.Text) != "" {
		c.Unfired.Outputs = []sessionstore.Output{
			{Type: sessionstore.OutputMessagePersistent, Content: response.Text, MessageRef: 0, Phase: "reply"},
		}
	}
}

func (s *Session) prepareStopResponse(
	r *runState,
	c *sessionstore.Commit,
	response *llmwire.Response,
	state *sessionstore.CompletionCheckState,
	wake bool,
) {
	reply := state != nil && state.ManagerReplyPending
	if wake {
		s.finalParts(c, response.Text, reply, true)
		r.result.Final = response.Text
		r.terminal = true
		return
	}
	if strings.TrimSpace(response.Text) == "" || response.FinishType == llmwire.FinishToolCalls {
		prepareEmptyStop(r, c, state)
		return
	}
	candidate := durableCandidateID(state)
	if candidate == 0 {
		c.Unfired.State.Candidate = &sessionstore.CandidateChange{Expected: 0, NextRef: 0}
		c.Unfired.Messages = []*transcript.Message{
			hostUserMessage(sessionprompt.RenderCompletionNudge(s.prompt.Todos.List())),
		}
		return
	}
	c.State.Candidate = &sessionstore.CandidateChange{Expected: candidate, NextRef: -1}
	c.State.ConfirmedAnswerID = &candidate
	text := response.Text
	if state.CandidateText != "" {
		text = state.CandidateText
	}
	s.finalParts(c, text, reply, false)
	r.result.Final = text
	r.terminal = true
}

func prepareEmptyStop(r *runState, c *sessionstore.Commit, state *sessionstore.CompletionCheckState) {
	next := 1
	if state != nil {
		next = state.EmptyStopStreak + 1
	}
	c.State.EmptyStopStreak = &next
	if next >= emptyResponseBreakThreshold {
		status := sessionstore.SessionStatusError
		c.Unfired.State.Status = &status
		c.Unfired.Outputs = []sessionstore.Output{{
			Type: sessionstore.OutputMessagePersistent, Content: sessionstore.EmptyStopTerminalNotice(next),
			Key: fmt.Sprintf("empty:%d:terminal", *c.State.Iteration), MessageRef: -1, ReleasesInput: true,
		}}
		r.terminal = true
		r.terminalState = true
		return
	}
	nudge := "You returned an empty response with no tool calls. Please continue working on the task, or explain what you need."
	if next == emptyResponseWarnThreshold {
		nudge = fmt.Sprintf(
			"[AUTOMATED WARNING: You have returned %d consecutive empty responses (no text, no tool calls). You MUST either use a tool or respond with text. If you cannot proceed, explain why.]",
			next,
		)
	}
	c.Unfired.Messages = []*transcript.Message{hostUserMessage(nudge)}
}

func (s *Session) prepareProjectionError(r *runState, c *sessionstore.Commit, cause error) {
	status := sessionstore.SessionStatusError
	c.Unfired.State.Status = &status
	c.Unfired.Outputs = []sessionstore.Output{{
		Type: sessionstore.OutputMessagePersistent, Content: projectionErrorNotice(cause),
		Key: fmt.Sprintf("projection-error:%d:terminal", *c.State.Iteration), MessageRef: -1, ReleasesInput: true,
	}}
	r.result.ErrorNotice = projectionErrorNotice(cause)
	r.terminal = true
	r.terminalState = true
}

func (s *Session) finalParts(c *sessionstore.Commit, text string, reply, yield bool) {
	if strings.TrimSpace(text) == "" {
		return
	}
	kind := sessionstore.OutputMessageReplaceable
	if reply {
		kind = sessionstore.OutputMessagePersistent
	}
	c.Unfired.Outputs = []sessionstore.Output{
		{
			Type:          kind,
			Content:       text,
			MessageRef:    0,
			Phase:         "final",
			ReleasesInput: true,
			FinalFooter:   &sessionstore.Footer{BackgroundYield: yield},
		},
	}
	no := false
	c.Unfired.State.ManagerReplyPending = &no
}

func durableCandidateID(state *sessionstore.CompletionCheckState) int64 {
	if state == nil || state.CandidateID == nil {
		return 0
	}
	return *state.CandidateID
}
