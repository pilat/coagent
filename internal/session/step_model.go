package session

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionprompt"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/transcript"
)

func (s *Session) modelStep(ctx context.Context, r *runState) error {
	response, sentCount, generation, err := s.callModel(ctx, r)
	if err != nil {
		return err
	}

	r.iterations++

	c, err := s.modelResponseCommit(r, response, sentCount, generation)
	if err != nil {
		return err
	}

	if response.FinishType == llmwire.FinishLength || response.FinishType == llmwire.FinishUnknown {
		r.directReply = false
		if err := s.prepareRejectedAttempt(ctx, r, &c, response); err != nil {
			return s.commitProjectionFailure(ctx, r, c, err)
		}
	} else if err := s.prepareAcceptedAttempt(ctx, r, &c, response); err != nil {
		return s.commitProjectionFailure(ctx, r, c, err)
	}

	return s.commitModelAttempt(ctx, r, c)
}

func (s *Session) callModel(ctx context.Context, r *runState) (*llmwire.Response, int, uint64, error) {
	activeTools := s.registry.List()
	if s.loopDetector.forceTextOnly {
		activeTools = nil
	}

	messages := s.ms.getMessages()
	generation := s.modelGeneration()

	response, err := s.chat(
		ctx,
		s.prompt.SystemPrompt(),
		repairTranscriptExcluding(messages, s.pendingExternalCallIDs()),
		tool.ToSchemas(activeTools),
	)
	if err != nil {
		r.result.ErrorNotice = "❌ LLM error: " + logger.Redact(err.Error())
		return nil, 0, 0, err
	}

	s.stamper.Touch()

	response.FinishType = normalizedFinishType(response.FinishType)
	if s.loopDetector.forceTextOnly && len(response.ToolCalls) == 0 {
		s.loopDetector.clearForceTextOnly()
	}

	return response, len(messages), generation, nil
}

func (s *Session) modelResponseCommit(
	r *runState,
	response *llmwire.Response,
	sentCount int,
	generation uint64,
) (sessionstore.Commit, error) {
	wire := llmwire.Message{
		Role: llmwire.RoleAssistant, Content: response.Text, ToolCalls: response.ToolCalls,
		ReasoningContent: response.ReasoningContent, ReasoningRaw: response.ReasoningRaw,
		CostUSD: response.CostUSD, Usage: response.Usage, FinishType: response.FinishType,
		ProviderFinishReason: response.ProviderFinishReason,
	}

	message, err := storedMessage(&wire)
	if err != nil {
		return sessionstore.Commit{}, err
	}

	c := s.newCommit()
	iteration := s.iterationOffset + r.iterations
	c.Messages = []*transcript.Message{message}
	c.ObserveBudget = true
	c.State.Iteration = &iteration

	if response.Usage != nil && response.Usage.PromptTokens > 0 {
		if model, ok := s.storeContextBaseline(response.Usage.PromptTokens, sentCount, generation); ok {
			c.State.ContextBaseline = &sessionstore.ContextBaseline{
				Model:        model,
				PromptTokens: response.Usage.PromptTokens,
				MessageCount: sentCount,
			}
		}
	}

	return c, nil
}

func (s *Session) prepareRejectedAttempt(
	ctx context.Context,
	r *runState,
	c *sessionstore.Commit,
	response *llmwire.Response,
) error {
	c.Messages[0].RejectedReason = sessionstore.RejectedReasonUnknownFinish
	notice := sessionstore.UnknownFinishTerminalError

	if response.FinishType == llmwire.FinishLength {
		c.Messages[0].RejectedReason = sessionstore.RejectedReasonOutputLength
		notice = sessionstore.OutputLengthTerminalError

		outstanding, err := s.store.HasOutstandingResponseRecovery(ctx, s.id)
		if err != nil {
			return fmt.Errorf("prepare rejected attempt: %w", err)
		}

		if !outstanding {
			ref := 0
			c.Unfired.Messages = []*transcript.Message{
				{Role: llmwire.RoleUser, Content: sessionstore.OutputLengthRecoveryPrompt, RetryOfRef: &ref},
			}
		}
	}

	if len(c.Unfired.Messages) == 0 {
		status := sessionstore.SessionStatusError
		c.Unfired.State.Status = &status
		c.Unfired.Outputs = []sessionstore.Output{{
			Type: sessionstore.OutputMessagePersistent, Content: sessionstore.IntegrityErrorNotice(notice),
			Key: fmt.Sprintf("integrity:%d:error", *c.State.Iteration), MessageRef: -1, ReleasesInput: true,
		}}
		r.terminal = true
		r.terminalState = true
		r.result.ErrorNotice = sessionstore.IntegrityErrorNotice(notice)
	}

	return nil
}

func (s *Session) prepareAcceptedAttempt(
	ctx context.Context,
	r *runState,
	c *sessionstore.Commit,
	response *llmwire.Response,
) error {
	state, err := s.store.LoadCompletionCheckState(ctx, s.id)
	if err != nil {
		return fmt.Errorf("prepare accepted attempt: %w", err)
	}

	zero := 0

	c.State.EmptyStopStreak = &zero
	if len(response.ToolCalls) > 0 {
		prepareToolResponse(r, c, response, state)
		return nil
	}

	wake, err := s.store.HasBackgroundWakeSource(ctx, s.id)
	if err != nil {
		s.prepareProjectionError(r, c, err)
		return nil
	}

	s.prepareStopResponse(r, c, response, state, wake)

	return nil
}

func (s *Session) commitModelAttempt(ctx context.Context, r *runState, c sessionstore.Commit) error {
	result, err := s.commit(ctx, c)
	if err != nil {
		return err
	}

	r.directReply = false

	r.result.BudgetFired = result.BudgetFired
	if result.BudgetFired {
		r.terminal = false
		r.terminalState = false

		return nil
	}

	if r.terminalState && r.result.ErrorNotice != "" {
		return errors.New(r.result.ErrorNotice)
	}

	return nil
}

func normalizedFinishType(value string) string {
	switch value {
	case llmwire.FinishStop, llmwire.FinishToolCalls, llmwire.FinishLength, llmwire.FinishUnknown:
		return value
	default:
		return llmwire.FinishUnknown
	}
}

func hostUserMessage(content string) *transcript.Message {
	return &transcript.Message{Role: llmwire.RoleUser, Content: content}
}

func projectionErrorNotice(err error) string {
	return fmt.Sprintf(
		"⚠️ Session error: %s\n\nThe session is still alive — send a message to continue.",
		logger.Redact(err.Error()),
	)
}

func (s *Session) commitProjectionFailure(ctx context.Context, r *runState, c sessionstore.Commit, cause error) error {
	status := sessionstore.SessionStatusError
	notice := projectionErrorNotice(cause)
	c.Unfired.State.Status = &status
	c.Unfired.Outputs = []sessionstore.Output{
		{
			Type:          sessionstore.OutputMessagePersistent,
			Content:       notice,
			Key:           fmt.Sprintf("projection-error:%d:terminal", s.iterationOffset+r.iterations),
			MessageRef:    -1,
			ReleasesInput: true,
		},
	}

	result, err := s.commit(ctx, c)
	if err != nil {
		return err
	}

	r.result.BudgetFired = result.BudgetFired
	if result.BudgetFired {
		return nil
	}

	r.terminal = true
	r.terminalState = true
	r.result.ErrorNotice = notice

	return cause
}

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
