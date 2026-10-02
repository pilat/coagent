package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

const (
	hardIterationCeiling        = 1000
	emptyResponseWarnThreshold  = 3
	emptyResponseBreakThreshold = 6
	compactionAttemptCap        = 3
	loopWarningTemplate         = "[LOOP WARNING: Low action diversity (%d%%). Your recent %d tool calls produced only %d unique outcomes.\n\nREQUIRED: Before your next tool call, explain in text WHY your current approach is not working and WHAT specifically you will change. Do not repeat the same strategy.]"
	loopBlockMessage            = "[BLOCKED: Tool execution blocked — you were warned about repetitive behavior but continued the same pattern. You MUST respond with text explaining your situation. No tool calls will be executed until you demonstrate a new approach.]"
	loopFailureWarningTemplate  = "[LOOP WARNING: The %s tool has returned the same error %d times in a row. Repeating the identical call will not help — fix the arguments or change your approach, or stop and explain the problem in text.]"
)

type RunResult struct {
	Suspended            bool
	BudgetFired          bool
	Final                string
	ErrorNotice          string
	DeferNoticeAnnounced bool
}

type runState struct {
	result             RunResult
	iterations         int
	terminal           bool
	terminalState      bool
	handledControl     bool
	directReply        bool
	backgroundInserted bool
	compactionFailures int
	autoCompactionOff  bool
}

func (s *svc) Run(ctx context.Context) (RunResult, error) {
	ctx = logger.With(ctx)
	if err := s.prepareRun(ctx); err != nil {
		return RunResult{}, err
	}
	r := &runState{}
	defer s.emit(sessionevent.Notification{Type: sessionevent.NotifyModelWorking, Attributes: map[string]any{"working": false}})
	defer s.startHeartbeat(ctx)()
	if !s.HasPendingExternalCall() {
		s.compactionDeferAnnounced = false
	}
	return s.finishRun(ctx, r, s.runIterations(ctx, r))
}

func (s *svc) prepareRun(ctx context.Context) error {
	s.refreshRegistrySections()
	index, err := tool.ActivationIndex(s.registry)
	if err != nil {
		return err
	}
	s.activationIndex = index
	return s.loadPendingActivation(ctx)
}

func (s *svc) runIterations(ctx context.Context, r *runState) error {
	for r.iterations < hardIterationCeiling {
		if err := ctx.Err(); err != nil {
			return err
		}
		again, err := s.runIteration(ctx, r)
		if err != nil || !again {
			return err
		}
	}
	return nil
}

func (s *svc) runIteration(ctx context.Context, r *runState) (bool, error) {
	accepted, err := s.boundaryStep(ctx, r)
	if err != nil {
		return false, err
	}
	if s.HasPendingExternalCall() {
		r.result.Suspended = true
		return false, nil
	}
	if len(s.pendingInLoopCalls()) > 0 {
		return s.runPendingTools(ctx, r)
	}
	if !accepted && (r.handledControl || !s.unansweredWork()) {
		return false, s.compactionStep(ctx, r)
	}
	admitted, err := s.admitModelStep(ctx, r)
	if err != nil || !admitted {
		return false, err
	}
	s.emit(sessionevent.Notification{Type: sessionevent.NotifyModelWorking, Attributes: map[string]any{"working": true}})
	if err := s.modelStep(ctx, r); err != nil {
		return false, err
	}
	if r.terminal || r.result.BudgetFired {
		return false, nil
	}
	return s.runPendingTools(ctx, r)
}

func (s *svc) runPendingTools(ctx context.Context, r *runState) (bool, error) {
	if calls := s.pendingInLoopCalls(); len(calls) > 0 {
		if err := s.toolStep(ctx, calls); err != nil {
			return false, err
		}
	}
	if s.suspended {
		r.result.Suspended = true
		return false, nil
	}
	return true, nil
}

func (s *svc) admitModelStep(ctx context.Context, r *runState) (bool, error) {
	if err := s.compactionStep(ctx, r); err != nil {
		return false, err
	}
	if s.budgetFired {
		r.result.BudgetFired = true
		return false, nil
	}
	fired, err := s.observeBudget(ctx)
	if err != nil {
		return false, err
	}
	r.result.BudgetFired = fired
	return !fired, nil
}

func (s *svc) emit(n sessionevent.Notification) {
	if s.events != nil {
		s.events.Emit(n)
	}
}

func (s *svc) newCommit() sessionstore.Commit {
	return sessionstore.Commit{SessionID: s.id, RootID: s.rootID, At: time.Now().UTC()}
}

func (s *svc) commit(ctx context.Context, c sessionstore.Commit) (*sessionstore.CommitResult, error) {
	for _, output := range append(append([]sessionstore.Output{}, c.Outputs...), c.Unfired.Outputs...) {
		if output.ReleasesInput {
			s.emit(sessionevent.Notification{Type: sessionevent.NotifyModelWorking, Attributes: map[string]any{"working": false}})
			break
		}
	}
	result, err := s.store.Commit(ctx, c)
	if err != nil {
		return nil, err
	}
	s.budgetFired = s.budgetFired || result.BudgetFired
	if err := s.ms.reloadMessages(ctx); err != nil {
		return nil, err
	}
	s.emitCommitted(result.Outputs, result.BudgetFired)
	return result, nil
}

func (s *svc) observeBudget(ctx context.Context) (bool, error) {
	_, fired, err := s.store.ObserveBudget(ctx, s.rootID, time.Now().UTC(), "")
	if err != nil {
		return false, err
	}
	return fired, nil
}

func (s *svc) loadPendingActivation(ctx context.Context) error {
	activation, err := s.store.PendingActivation(ctx, s.id)
	if errors.Is(err, sessionstore.ErrActivationNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if activation == nil {
		return nil
	}
	s.currentActivation = &tool.ActivationGrant{SessionID: activation.SessionID, InputID: activation.InputID, ToolID: activation.ToolID, Command: activation.Command, ToolCallID: activation.ToolCallID}
	return nil
}

func (s *svc) expireActivation(ctx context.Context, suspended bool) error {
	grant := s.currentActivation
	if grant == nil || grant.ToolCallID != "" {
		return nil
	}
	if suspended {
		for _, call := range s.PendingExternalCalls() {
			if call.Name == grant.ToolID {
				return nil
			}
		}
	}
	c := s.newCommit()
	c.Activation = &sessionstore.ActivationChange{InputID: grant.InputID, State: sessionstore.ActivationExpired, ToolID: grant.ToolID, Command: grant.Command}
	c.Outputs = []sessionstore.Output{{Type: sessionstore.OutputMessagePersistent, Content: grant.Command + " was not changed", Key: fmt.Sprintf("input:%d:activation:expired", grant.InputID), MessageRef: -1, ReleasesInput: true}}
	if _, err := s.commit(ctx, c); err != nil {
		return err
	}
	s.currentActivation = nil
	return nil
}

func (s *svc) emitCommitted(outputs []*sessionstore.OutputCommit, fired bool) {
	if fired {
		s.emit(sessionevent.Notification{Type: sessionevent.NotifyModelWorking, Attributes: map[string]any{"working": false}})
	}
	for _, output := range outputs {
		if output != nil && !output.Existing && output.Content != "" {
			s.emit(sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: output.Content})
		}
	}
}
