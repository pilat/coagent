package daemon

import (
	"context"
	"fmt"
	"slices"

	"github.com/pilat/coagent/internal/orchestrator"
	"github.com/pilat/coagent/internal/sessionstore"
)

// SpawnForApp creates an empty ordinary root under the app's delegated authority.
func (s *Service) SpawnForApp(ctx context.Context, app, workDir, worktree, model string) (int64, error) {
	id, _, err := (orchestratorHost{s: s}).spawn(ctx, delegate{app: app}, orchestrator.SpawnRequest{
		Project: workDir, Worktree: worktree, Model: model,
	})

	return id, err
}

// SendForApp persists a sent message without granting human authority or arming a watch.
func (s *Service) SendForApp(ctx context.Context, app string, sessionID int64, message, key string) (int64, error) {
	if key != "" {
		key = "app:" + key
	}

	id, _, err := (orchestratorHost{s: s}).send(ctx, delegate{app: app}, sessionID, message, key)

	return id, err
}

// StopForApp stops only a root started by this app, following replacements.
func (s *Service) StopForApp(ctx context.Context, app string, sessionID int64) (int64, error) {
	ended, err := (orchestratorHost{s: s}).stop(ctx, delegate{app: app}, sessionID)

	return ended.ID, err
}

// KillForApp kills only a root started by this app, following replacements.
func (s *Service) KillForApp(ctx context.Context, app string, sessionID int64) (int64, error) {
	ended, err := (orchestratorHost{s: s}).kill(ctx, delegate{app: app}, sessionID)

	return ended.ID, err
}

// TurnOver reads the newest root's durable obligations and its current runner.
func (s *Service) TurnOver(ctx context.Context, sessionID int64) (int64, bool, error) {
	chain, err := s.store.ReplacementChain(ctx, sessionID)
	if err != nil {
		return 0, false, fmt.Errorf("resolve turn outcome: %w", err)
	}

	id := chain[len(chain)-1]

	record, err := s.store.GetSession(ctx, id)
	if err != nil {
		return 0, false, fmt.Errorf("load turn outcome: %w", err)
	}

	if record.KilledAt != nil || record.Status == sessionstore.SessionStatusKilled ||
		record.Status == sessionstore.SessionStatusTerminating {
		return id, true, nil
	}

	if s.runners.waitingForAdmission(id) {
		return id, false, nil
	}

	state, err := (orchestratorHost{s: s}).sessionState(ctx, record)
	if err != nil {
		return id, false, err
	}

	switch state {
	case sessionStateRunning, sessionStateWaiting, string(sessionstore.SessionStatusStopping):
		return id, false, nil
	case sessionStateParked:
		return id, true, nil
	case string(sessionstore.SessionStatusError):
		over, err := s.erroredTurnOver(ctx, record)

		return id, over, err
	}

	over, err := s.outcomeQuiet(ctx, id)

	return id, over, err
}

func (s *Service) erroredTurnOver(ctx context.Context, record *sessionstore.SessionRecord) (bool, error) {
	inputs, err := s.store.ListPending(ctx, record.ID)
	if err != nil {
		return false, fmt.Errorf("list errored root inputs: %w", err)
	}

	// Input received during the failed run does not restart it.
	fresh := slices.DeleteFunc(inputs, func(input *sessionstore.InboxInput) bool {
		return !input.ReceivedAt.After(record.UpdatedAt)
	})

	return !wakeWorthy(record, fresh), nil
}
