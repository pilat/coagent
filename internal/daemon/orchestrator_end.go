package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/orchestrator"
	"github.com/pilat/coagent/internal/sessionstore"
)

func (h orchestratorHost) StopSession(ctx context.Context, callerID, targetID int64) (orchestrator.Ended, error) {
	caller, target, err := h.endTarget(ctx, callerID, targetID)
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

	return ended, h.command(ctx, caller, target.ID, stopCommand)
}

func (h orchestratorHost) KillSession(ctx context.Context, callerID, targetID int64) (orchestrator.Ended, error) {
	caller, target, err := h.endTarget(ctx, callerID, targetID)
	if err != nil {
		return orchestrator.Ended{}, err
	}

	ended := orchestrator.Ended{ID: target.ID}
	if target.KilledAt != nil || target.Status == sessionstore.SessionStatusKilled {
		return ended, nil
	}

	ended.Changed = true

	return ended, h.command(ctx, caller, target.ID, killCommand)
}

func (h orchestratorHost) endTarget(
	ctx context.Context, callerID, targetID int64,
) (*sessionstore.SessionRecord, *sessionstore.SessionRecord, error) {
	caller, owner, err := h.caller(ctx, callerID)
	if err != nil {
		return nil, nil, err
	}

	if err := h.rootTarget(ctx, owner, targetID); err != nil {
		return nil, nil, err
	}

	chain, err := h.s.store.ReplacementChain(ctx, targetID, owner)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve end target: %w", err)
	}

	target, err := h.s.store.GetSession(ctx, chain[len(chain)-1])
	if err != nil {
		return nil, nil, fmt.Errorf("load end target: %w", err)
	}

	lineage, err := h.s.store.ReplacementChain(ctx, callerID, owner)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve ending session: %w", err)
	}

	if !slices.Contains(lineage, controllerapi.SpawnedBy(target.Attributes)) {
		return nil, nil, fmt.Errorf(
			"session #%d was not started by this session; you can stop or kill only sessions you spawned", target.ID)
	}

	return caller, target, nil
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
	caller *sessionstore.SessionRecord,
	targetID int64,
	content string,
) error {
	attrs, err := h.provenance(ctx, caller)
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

func commandSender(input *sessionstore.InboxInput) int64 {
	switch id := input.Attributes[sessionstore.InputAttributeSenderSessionID].(type) {
	case int64:
		return id
	case float64:
		return int64(id)
	default:
		return 0
	}
}

func commandAcknowledgement(input *sessionstore.InboxInput, content string) string {
	if sender := commandSender(input); sender != 0 {
		return fmt.Sprintf(
			"%s — asked by #%d %s",
			content,
			sender,
			input.Attributes[sessionstore.InputAttributeSenderName],
		)
	}

	return content
}
