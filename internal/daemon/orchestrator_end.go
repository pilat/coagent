package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/orchestrator"
	"github.com/pilat/coagent/internal/sessionstore"
)

func (h orchestratorHost) StopSession(ctx context.Context, callerID, targetID int64) (orchestrator.Ended, error) {
	caller, err := h.caller(ctx, callerID)
	if err != nil {
		return orchestrator.Ended{}, err
	}

	return h.stop(ctx, delegate{root: caller}, targetID)
}

func (h orchestratorHost) KillSession(ctx context.Context, callerID, targetID int64) (orchestrator.Ended, error) {
	caller, err := h.caller(ctx, callerID)
	if err != nil {
		return orchestrator.Ended{}, err
	}

	return h.kill(ctx, delegate{root: caller}, targetID)
}

func (h orchestratorHost) stop(ctx context.Context, from delegate, targetID int64) (orchestrator.Ended, error) {
	target, err := h.endTarget(ctx, from, targetID)
	if err != nil {
		return orchestrator.Ended{}, err
	}

	if target.KilledAt != nil || target.Status == sessionstore.SessionStatusKilled {
		return orchestrator.Ended{}, fmt.Errorf("session #%d is killed", target.ID)
	}

	armed, err := h.budgetArmed(ctx, target.ID)
	if err != nil {
		return orchestrator.Ended{}, err
	}

	if armed {
		return orchestrator.Ended{}, fmt.Errorf(
			"session #%d runs under a budget the human set; leave it to the human, or end it for good with kill_session",
			target.ID,
		)
	}

	state, err := h.sessionState(ctx, target)
	if err != nil {
		return orchestrator.Ended{}, err
	}

	ended := orchestrator.Ended{ID: target.ID, State: state, Running: state == sessionStateRunning}
	if target.Status == sessionstore.SessionStatusStopped || target.Status == sessionstore.SessionStatusStopping ||
		target.Status == sessionstore.SessionStatusTerminating {
		if state != sessionStateParked {
			ended.State = string(target.Status)
		}

		return ended, nil
	}

	ended.Changed, err = h.stopNeeded(ctx, target.ID, state)
	if err != nil || !ended.Changed {
		return ended, err
	}

	return ended, h.command(ctx, from, target.ID, stopCommand)
}

func (h orchestratorHost) kill(ctx context.Context, from delegate, targetID int64) (orchestrator.Ended, error) {
	target, err := h.endTarget(ctx, from, targetID)
	if err != nil {
		return orchestrator.Ended{}, err
	}

	ended := orchestrator.Ended{ID: target.ID}
	if target.KilledAt != nil || target.Status == sessionstore.SessionStatusKilled {
		return ended, nil
	}

	ended.Changed = true

	return ended, h.command(ctx, from, target.ID, killCommand)
}

func (h orchestratorHost) endTarget(
	ctx context.Context, from delegate, targetID int64,
) (*sessionstore.SessionRecord, error) {
	if err := h.rootTarget(ctx, targetID); err != nil {
		return nil, err
	}

	chain, err := h.s.store.ReplacementChain(ctx, targetID)
	if err != nil {
		return nil, fmt.Errorf("resolve end target: %w", err)
	}

	target, err := h.s.store.GetSession(ctx, chain[len(chain)-1])
	if err != nil {
		return nil, fmt.Errorf("load end target: %w", err)
	}

	var lineage []int64
	if from.root != nil {
		lineage, err = h.s.store.ReplacementChain(ctx, from.root.ID)
		if err != nil {
			return nil, fmt.Errorf("resolve ending session: %w", err)
		}
	}

	if !from.started(lineage, target) {
		if from.root == nil {
			return nil, fmt.Errorf(
				"session #%d was not started by this app; you can stop or kill only sessions you started", target.ID)
		}

		return nil, fmt.Errorf(
			"session #%d was not started by this session; you can stop or kill only sessions you spawned", target.ID)
	}

	return target, nil
}

func (h orchestratorHost) stopNeeded(ctx context.Context, targetID int64, state string) (bool, error) {
	if state == sessionStateRunning || state == sessionStateWaiting {
		return true, nil
	}

	rows, err := h.s.store.ListPending(ctx, targetID)
	if err != nil {
		return false, fmt.Errorf("load stop target inputs: %w", err)
	}

	for _, row := range rows {
		if row.Source == sessionstore.InputSourceUser || row.Source == sessionstore.InputSourceAgent {
			return true, nil
		}
	}

	obligation, err := h.s.store.HasBackgroundObligationByRoot(ctx, targetID)
	if err != nil {
		return false, fmt.Errorf("load stop target obligations: %w", err)
	}

	return obligation, nil
}

func (h orchestratorHost) budgetArmed(ctx context.Context, targetID int64) (bool, error) {
	record, err := h.s.store.Get(ctx, targetID)
	if errors.Is(err, budget.ErrNotFound) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("load stop target budget: %w", err)
	}

	return record.State == budget.Armed, nil
}

func (h orchestratorHost) command(
	ctx context.Context,
	from delegate,
	targetID int64,
	content string,
) error {
	attrs, err := h.provenance(ctx, from)
	if err != nil {
		return err
	}

	queued, err := h.s.store.Enqueue(ctx, sessionstore.Input{
		SessionID:  targetID,
		Source:     sessionstore.InputSourceUser,
		Content:    content,
		Attributes: attrs,
	})
	if errors.Is(err, sessionstore.ErrSessionNotAcceptingInput) {
		return fmt.Errorf("session #%d is stopping; try again shortly", targetID)
	}

	if err != nil {
		return fmt.Errorf("enqueue session command: %w", err)
	}

	_, err = h.s.handleGenericCommand(context.WithoutCancel(ctx), queued.Input)

	return err
}

func commandActor(input *sessionstore.InboxInput) orchestrator.Actor {
	app, _ := input.Attributes[sessionstore.InputAttributeSenderApp].(string)
	actor := orchestrator.Actor{App: app}

	switch id := input.Attributes[sessionstore.InputAttributeSenderSessionID].(type) {
	case int64:
		actor.Session = id
	case float64:
		actor.Session = int64(id)
	}

	return actor
}

func commandAcknowledgement(input *sessionstore.InboxInput, content string) string {
	actor := commandActor(input)
	if actor.Session != 0 {
		return fmt.Sprintf(
			"%s — asked by #%d %s",
			content,
			actor.Session,
			input.Attributes[sessionstore.InputAttributeSenderName],
		)
	}

	if actor.App != "" {
		return content + " — asked by app " + actor.App
	}

	return content
}
