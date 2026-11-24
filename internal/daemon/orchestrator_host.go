package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/orchestrator"
	"github.com/pilat/coagent/internal/sessionstore"
)

const (
	sessionStateRunning = "running"
	sessionStateWaiting = "waiting"
	sessionStateParked  = "parked"
	sessionStateIdle    = "idle"
)

var _ orchestrator.Host = orchestratorHost{}

type orchestratorHost struct{ s *Service }

func (h orchestratorHost) ListSessions(
	ctx context.Context,
	callerID int64,
	filter string,
) ([]orchestrator.Entry, int, error) {
	_, err := h.caller(ctx, callerID)
	if err != nil {
		return nil, 0, err
	}

	rows, err := h.s.store.ChatSessions(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("list sessions: %w", err)
	}

	rows = slices.DeleteFunc(rows, func(row sessionstore.ChatSession) bool {
		return !strings.Contains(strings.ToLower(row.Name), strings.ToLower(filter))
	})

	states, err := h.s.store.WatchStates(ctx, callerID)
	if err != nil {
		return nil, 0, fmt.Errorf("load listed session watches: %w", err)
	}

	lineage, err := h.s.store.ReplacementChain(ctx, callerID)
	if err != nil {
		return nil, 0, fmt.Errorf("resolve listing session: %w", err)
	}

	entries := make([]orchestrator.Entry, 0, min(20, len(rows)))

	for _, row := range rows[:min(20, len(rows))] {
		record, err := h.s.store.GetSession(ctx, row.ID)
		if err != nil {
			return nil, 0, fmt.Errorf("load listed session: %w", err)
		}

		state, err := h.sessionState(ctx, record)
		if err != nil {
			return nil, 0, err
		}

		entry := orchestrator.Entry{
			ID:         row.ID,
			Name:       row.Name,
			State:      state,
			Activity:   row.Activity,
			Self:       row.ID == callerID,
			Spawned:    slices.Contains(lineage, controllerapi.SpawnedBy(record.Attributes)),
			Subscribed: states[row.ID].Subscribed,
			Armed:      states[row.ID].Armed,
			Signalled:  states[row.ID].Signalled,
		}
		entries = append(entries, entry)
	}

	return entries, len(rows), nil
}

func (h orchestratorHost) SendToSession(
	ctx context.Context,
	callerID, targetID int64,
	message string,
) (int64, bool, error) {
	caller, err := h.caller(ctx, callerID)
	if err != nil {
		return 0, false, err
	}

	target, err := h.sendTarget(ctx, targetID)
	if err != nil {
		return 0, false, err
	}

	if target.ID == callerID {
		return 0, false, errors.New("cannot send to this session itself")
	}

	parked, err := h.parked(ctx, target.ID)
	if err != nil {
		return 0, false, err
	}

	if parked {
		return 0, false, fmt.Errorf(
			"session #%d is parked at its budget checkpoint; only the human can resume it",
			target.ID,
		)
	}

	attrs, err := h.provenance(ctx, caller)
	if err != nil {
		return 0, false, err
	}

	var unread bool

	err = h.s.store.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := sessionstore.EnqueueTx(ctx, tx, sessionstore.Input{
			SessionID: target.ID, Source: sessionstore.InputSourceAgent, Content: message, Attributes: attrs,
		})
		if err != nil {
			return fmt.Errorf("enqueue sent message: %w", err)
		}

		unread, err = sessionstore.ArmWatchTx(ctx, tx, callerID, target.ID, false)
		if err != nil {
			return fmt.Errorf("arm sent session watch: %w", err)
		}

		return nil
	})
	if errors.Is(err, sessionstore.ErrSessionNotAcceptingInput) {
		return 0, false, fmt.Errorf("session #%d is stopping; try again shortly", target.ID)
	}

	if err != nil {
		return 0, false, fmt.Errorf("send to session: %w", err)
	}

	return target.ID, unread, nil
}

func (h orchestratorHost) caller(ctx context.Context, id int64) (*sessionstore.SessionRecord, error) {
	record, err := h.s.store.GetSession(ctx, id)
	if err != nil || record == nil {
		return nil, errors.New("orchestrator mode is off for this session")
	}

	if record.ParentID != 0 {
		return nil, errors.New("orchestrator mode is off for this session")
	}

	if record.KilledAt != nil || record.Status == sessionstore.SessionStatusKilled ||
		record.Status == sessionstore.SessionStatusTerminating {
		return nil, errors.New("orchestrator mode is off for this session")
	}

	if !controllerapi.OrchestratorMode(record.Attributes) {
		return nil, errors.New("orchestrator mode is off for this session")
	}

	return record, nil
}

func (h orchestratorHost) rootTarget(ctx context.Context, id int64) error {
	record, err := h.s.store.GetSession(ctx, id)
	if err != nil || record == nil {
		return fmt.Errorf("session #%d not found", id)
	}

	if record.ParentID != 0 {
		return fmt.Errorf("session #%d is a subagent, not a root session", id)
	}

	return nil
}

func (h orchestratorHost) sendTarget(ctx context.Context, id int64) (*sessionstore.SessionRecord, error) {
	if err := h.rootTarget(ctx, id); err != nil {
		return nil, err
	}

	resolved, err := h.s.store.ResolveReplacement(ctx, id)
	if errors.Is(err, sessionstore.ErrSessionNotAcceptingInput) {
		return nil, fmt.Errorf("session #%d is killed", id)
	}

	if err != nil {
		return nil, fmt.Errorf("resolve sent message target: %w", err)
	}

	record, err := h.s.store.GetSession(ctx, resolved)
	if err != nil {
		return nil, fmt.Errorf("reload sent message target: %w", err)
	}

	if record.KilledAt != nil || record.Status == sessionstore.SessionStatusKilled ||
		record.Status == sessionstore.SessionStatusTerminating {
		return nil, fmt.Errorf("session #%d is killed", id)
	}

	return record, nil
}

func (h orchestratorHost) parked(ctx context.Context, id int64) (bool, error) {
	if h.s.parks.active(id) {
		return true, nil
	}

	record, err := h.s.store.Get(ctx, id)
	if errors.Is(err, budget.ErrNotFound) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("load sent message budget: %w", err)
	}

	return record.State == budget.Fired, nil
}

func (h orchestratorHost) sessionState(ctx context.Context, record *sessionstore.SessionRecord) (string, error) {
	if h.s.HasActiveLoop(record.ID) {
		return sessionStateRunning, nil
	}

	parked, err := h.parked(ctx, record.ID)
	if err != nil {
		return "", err
	}

	if parked {
		return sessionStateParked, nil
	}

	switch record.Status {
	case sessionstore.SessionStatusActive, sessionstore.SessionStatusCompleted:
		return sessionStateIdle, nil
	case sessionstore.SessionStatusSuspended:
		return sessionStateWaiting, nil
	case sessionstore.SessionStatusError, sessionstore.SessionStatusStopping, sessionstore.SessionStatusStopped,
		sessionstore.SessionStatusTerminating, sessionstore.SessionStatusKilled:
		return string(record.Status), nil
	default:
		return string(record.Status), nil
	}
}

func (h orchestratorHost) provenance(ctx context.Context, from *sessionstore.SessionRecord) (map[string]any, error) {
	project, err := h.s.store.GetProjectName(ctx, from.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("load sender project: %w", err)
	}

	title, _ := from.Attributes[controllerapi.SessionAttributeTitle].(string)

	return map[string]any{
		sessionstore.InputAttributeSenderSessionID: from.ID,
		sessionstore.InputAttributeSenderName:      sessionstore.ChatDisplayName(project, title),
		sessionstore.InputAttributeSenderTitle:     title,
	}, nil
}
