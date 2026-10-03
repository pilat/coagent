package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/sessionstore"
)

func (s *Session) finishRun(ctx context.Context, r *runState, runErr error) (RunResult, error) {
	if r.iterations >= hardIterationCeiling && runErr == nil {
		runErr = fmt.Errorf("maximum iterations (%d) reached", hardIterationCeiling)
	}
	r.result.DeferNoticeAnnounced = s.compactionDeferAnnounced
	r.result.BudgetFired = r.result.BudgetFired || s.budgetFired
	if ctx.Err() != nil {
		return r.result, s.cancelRunActivation(ctx, runErr)
	}
	if err := s.expireActivation(ctx, r.result.Suspended); err != nil {
		runErr = errors.Join(runErr, err)
	}
	if !r.terminalState {
		runErr = s.commitRunState(ctx, r, runErr)
	}
	return r.result, runErr
}

func (s *Session) cancelRunActivation(ctx context.Context, runErr error) error {
	grant := s.currentActivation
	if grant == nil || grant.ToolCallID != "" {
		return runErr
	}
	c := s.newCommit()
	c.Activation = &sessionstore.ActivationChange{
		InputID: grant.InputID, State: sessionstore.ActivationExpired,
		ToolID: grant.ToolID, Command: grant.Command,
	}
	_, err := s.store.Commit(context.WithoutCancel(ctx), c)
	if err != nil && !errors.Is(err, sessionstore.ErrSessionStopping) {
		return errors.Join(runErr, err)
	}
	return runErr
}

func (s *Session) commitRunState(ctx context.Context, r *runState, runErr error) error {
	status := s.runStatus(r, runErr)
	iteration := s.iterationOffset + r.iterations
	todoData, err := json.Marshal(s.prompt.Todos.List())
	if err != nil {
		return errors.Join(runErr, err)
	}
	raw := json.RawMessage(todoData)
	c := s.newCommit()
	c.State = sessionstore.StatePatch{Status: &status, Iteration: &iteration, TodoItems: &raw}
	if runErr != nil {
		if r.result.ErrorNotice == "" {
			r.result.ErrorNotice = projectionErrorNotice(runErr)
		}
		c.Outputs = []sessionstore.Output{{
			Type: sessionstore.OutputMessagePersistent, Content: r.result.ErrorNotice,
			Key: fmt.Sprintf("run:%d:error", iteration), MessageRef: -1, ReleasesInput: true,
		}}
	}
	_, err = s.commit(ctx, c)
	if err != nil {
		return errors.Join(runErr, err)
	}
	return runErr
}

func (s *Session) runStatus(r *runState, runErr error) sessionstore.SessionStatus {
	if runErr != nil {
		return sessionstore.SessionStatusError
	}
	if s.preserveStopped {
		return sessionstore.SessionStatusStopped
	}
	if r.result.Suspended || r.result.BudgetFired {
		return sessionstore.SessionStatusSuspended
	}
	return sessionstore.SessionStatusCompleted
}
