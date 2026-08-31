//nolint:wrapcheck // Adapter preserves budget/store sentinel errors for the session loop.; nosemgrep: semgrep.coagent-no-preamble-before-package
package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/transcript"
)

type sessionBudgetGate struct {
	daemon    *svc
	service   budget.Service
	store     sessionstore.AgentRuntimeStore
	sessionID int64
	rootID    int64
}

const budgetParkRequested = "requested"

//nolint:wsl_v5 // Admission observes durable usage before another call.
func (g *sessionBudgetGate) Admit(ctx context.Context, now time.Time) error {
	_, _, cost, err := g.store.GetSessionTreeUsage(ctx, g.rootID)
	if err != nil {
		return fmt.Errorf("load budget admission usage: %w", err)
	}
	record, fired, err := g.service.Observe(ctx, g.rootID, cost, now, "")
	if err != nil {
		return fmt.Errorf("observe budget admission: %w", err)
	}
	if fired {
		if record.ParkPhase == budgetParkRequested {
			g.daemon.startBudgetPark(record)
		}

		return session.ErrBudgetCheckpoint
	}
	if err := g.service.Admit(ctx, g.rootID, now); err != nil {
		return fmt.Errorf("admit budgeted request: %w", err)
	}

	return nil
}

//nolint:wsl_v5 // Commit and park scheduling form one response boundary.
func (g *sessionBudgetGate) PersistResponse(
	ctx context.Context,
	message *transcript.Message,
	directReply string,
) (int64, bool, bool, error) {
	result, err := g.store.InsertBudgetedResponse(ctx, sessionstore.BudgetedResponse{
		SessionID: g.sessionID, RootID: g.rootID, Message: message,
		DirectReply: directReply, ObservedAt: time.Now().UTC(),
	})
	if err != nil {
		return 0, false, false, err
	}
	if result.Fired && result.Budget.ParkPhase == budgetParkRequested {
		g.daemon.startBudgetPark(result.Budget)
	}

	return result.MessageID, result.Fired, result.ReplyPublished, nil
}

func (g *sessionBudgetGate) Observe(ctx context.Context) (bool, error) {
	_, _, cost, err := g.store.GetSessionTreeUsage(ctx, g.rootID)
	if err != nil {
		return false, err
	}

	record, fired, err := g.service.Observe(ctx, g.rootID, cost, time.Now().UTC(), "")
	if fired && err == nil && record.ParkPhase == budgetParkRequested {
		g.daemon.startBudgetPark(record)
	}

	return fired, err
}

//nolint:wsl_v5 // Store selection, commit, and park scheduling are one boundary.
func (g *sessionBudgetGate) PersistCompaction(
	ctx context.Context,
	compaction sessionstore.BudgetedCompaction,
) ([]int64, bool, error) {
	compaction.SessionID = g.sessionID
	compaction.RootID = g.rootID
	result, err := g.store.ReplaceCompactedMessagesBudgeted(ctx, compaction)
	if err != nil {
		return nil, false, err
	}
	if result.Fired && result.Budget.ParkPhase == budgetParkRequested {
		g.daemon.startBudgetPark(result.Budget)
	}

	return result.MessageIDs, result.Fired, nil
}

func (s *svc) registerBudgetTool(
	ctx context.Context,
	record *sessionstore.SessionRecord,
	sess session.Service,
) {
	if s.budgetSvc == nil || record.ParentID != 0 {
		return
	}

	registerLogged(ctx, sess, budget.NewTool(s.budgetSvc, record.ID, s.modelHasPricing(record.Model)))
}

func (s *svc) modelHasPricing(modelID string) bool {
	for _, model := range s.modelEntries {
		if model.ID == modelID {
			return model.Pricing != nil
		}
	}

	return false
}

func sessionRootID(record *sessionstore.SessionRecord) int64 {
	if record.RootID != 0 {
		return record.RootID
	}

	return record.ID
}

func (s *svc) releaseArmedBudget(ctx context.Context, rootID int64, reason string) error {
	if s.budgetSvc == nil {
		return nil
	}

	record, err := s.budgetSvc.Get(ctx, rootID)
	if errors.Is(err, sessionstore.ErrBudgetNotFound) ||
		(err == nil && record.State != sessionstore.BudgetArmed) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("load budget for terminal release: %w", err)
	}

	if _, err := s.budgetSvc.Release(ctx, rootID, record.Generation, reason); err != nil {
		return fmt.Errorf("release terminal budget: %w", err)
	}

	return nil
}
