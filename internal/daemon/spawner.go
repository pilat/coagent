package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

// Spawn creates a child session + durable link and starts it running in the
// background. It never waits: the child id is returned immediately.
func (s *svc) Spawn(ctx context.Context, req spawnRequest) (childResult, error) {
	s.treeMu.Lock()
	defer s.treeMu.Unlock()

	childID, workDir, projectID, err := s.createChildSession(ctx, req)
	if err != nil {
		return childResult{}, err
	}

	// Start the child loop. Detached ctx so the parent's tool-call ctx (which may
	// time out / cancel) never kills the child (Appendix G6).
	if err := s.ensureRunner(context.WithoutCancel(ctx), childID, workDir, projectID, nil); err != nil {
		return childResult{}, fmt.Errorf("start child runner: %w", err)
	}

	return childResult{ChildID: childID, State: subagent.StateSpawned}, nil
}

// createChildSession validates the spawn request (nesting depth), then durably
// creates the child session row + its subagent_links row.
// Returns the child id plus the parent's workdir/project for runner start. The
// agent type was already validated by the task tool against the session's set.
func (s *svc) createChildSession(ctx context.Context, req spawnRequest) (int64, string, int64, error) {
	parentRec, err := s.sessionStore.GetSession(ctx, req.ParentID)
	if err != nil {
		return 0, "", 0, fmt.Errorf("parent session %d: %w", req.ParentID, err)
	}

	// Nesting cap: root → child → grandchild (Decision 10). Reject deeper.
	depth, err := s.childDepth(ctx, req.ParentID)
	if err != nil {
		return 0, "", 0, err
	}

	if depth >= maxSubagentDepth {
		return 0, "", 0, fmt.Errorf(
			"subagent nesting limit reached (depth %d): do this work inline instead of delegating further",
			depth,
		)
	}

	workDir, err := s.store.GetProjectWorkDir(ctx, parentRec.ProjectID)
	if err != nil {
		return 0, "", 0, fmt.Errorf("resolve parent workdir: %w", err)
	}

	// rootID is derived from the parent's own record (single source of truth):
	// a root parent stores RootID=0, so its children root at the parent itself.
	rootID := parentRec.RootID
	if rootID == 0 {
		rootID = req.ParentID
	}

	model := s.resolveChildModel(req, parentRec)
	if budgets, ok := s.sessionStore.(sessionstore.BudgetStore); ok {
		budgetRecord, budgetErr := budgets.GetBudget(ctx, rootID)
		if budgetErr == nil && budgetRecord.State == sessionstore.BudgetArmed &&
			budgetRecord.CostLimitUSD != nil && !s.modelHasPricing(model) {
			return 0, "", 0, errors.New(
				"cannot spawn an armed budget tree onto a model without catalog pricing",
			)
		}

		if budgetErr != nil && !errors.Is(budgetErr, sessionstore.ErrBudgetNotFound) {
			return 0, "", 0, fmt.Errorf("load root budget for child model: %w", budgetErr)
		}
	}

	reasoning, err := s.resolveChildEffort(model, req.ReasoningLevel, parentRec.ReasoningLevel)
	if err != nil {
		return 0, "", 0, err
	}

	childID, err := s.sessionStore.CreateSubagentWithLink(ctx, sessionstore.SubagentCreate{
		ProjectID:      parentRec.ProjectID,
		ParentID:       req.ParentID,
		RootID:         rootID,
		AgentType:      req.AgentType,
		Model:          model,
		ReasoningLevel: reasoning,
		TaskCallID:     req.TaskCallID,
		Blocking:       req.Blocking,
		Depth:          depth,
		State:          subagent.StateSpawned,
		TimeoutSec:     req.TimeoutSec,
		InitialInput:   req.Prompt,
	})
	if err != nil {
		return 0, "", 0, fmt.Errorf("create subagent with link: %w", err)
	}

	return childID, workDir, parentRec.ProjectID, nil
}

// Result returns a snapshot of a child's state and (once terminal) its output.
func (s *svc) Result(ctx context.Context, childID int64) (childResult, error) {
	return s.childSnapshot(ctx, childID)
}

// SendToChild durably appends a follow-up to a child's inbox. If the
// current activation is still running, the same runner consumes it at the next
// loop boundary. If it already ended, the previous completion is delivered
// first; persistCompletion then re-arms and starts the next activation. A
// completed foreground child becomes a background continuation because its
// original task call has already been resolved.
func (s *svc) SendToChild(ctx context.Context, childID int64, msg string) error {
	link, err := s.links.GetLink(ctx, childID)
	if err != nil {
		return fmt.Errorf("load subagent link: %w", err)
	}

	if link == nil {
		return fmt.Errorf("subagent %d not found", childID)
	}

	if link.State == subagent.StateKilled {
		return fmt.Errorf("subagent %d is killed", childID)
	}

	if _, err := s.inboxStore.EnqueueInput(ctx, childID, sessionstore.InputSourceAgent, msg); err != nil {
		return fmt.Errorf("persist subagent follow-up: %w", err)
	}

	// Re-read after enqueue. The child may have crossed its terminal boundary
	// while the durable write committed.
	link, err = s.links.GetLink(ctx, childID)
	if err != nil {
		return fmt.Errorf("reload subagent link: %w", err)
	}

	if link == nil {
		return fmt.Errorf("subagent %d disappeared after accepting follow-up", childID)
	}

	if link.State == subagent.StateStopped {
		rec, recErr := s.sessionStore.GetSession(ctx, childID)
		if recErr != nil {
			return fmt.Errorf("load stopped subagent session: %w", recErr)
		}

		if rec.Status == sessionstore.SessionStatusStopping {
			return nil // /stop won; it will cancel this accepted input
		}

		if err := s.links.ResetLinkRunning(ctx, childID); err != nil {
			return fmt.Errorf("resume stopped subagent link: %w", err)
		}

		if err := s.sessionStore.UpdateSessionStatus(ctx, childID, sessionstore.SessionStatusActive); err != nil {
			return fmt.Errorf("resume stopped subagent session: %w", err)
		}

		return s.ensureSessionRunner(context.WithoutCancel(ctx), childID)
	}

	if link.Terminal() {
		if link.DeliveredAt != 0 {
			return s.rearmChildAfterDelivery(context.WithoutCancel(ctx), childID)
		}

		s.deliverCompletionToParent(context.WithoutCancel(ctx), *link)

		return nil
	}

	return s.ensureSessionRunner(context.WithoutCancel(ctx), childID)
}

// LinkPending reports whether a link already exists for this task call — the
// resume idempotency check. An unresolved task tool_use that already has a link
// must re-suspend (never re-fork): its completion is either in flight or
// terminal-but-not-yet-delivered. (A delivered completion fills the tool_use, so
// it is resolved and never re-executed — this is only reached for unresolved calls.)
func (s *svc) LinkPending(ctx context.Context, parentID int64, taskCallID string) (bool, error) {
	link, err := s.links.GetLinkByTaskCallID(ctx, parentID, taskCallID)
	if err != nil {
		return false, fmt.Errorf("check pending link: %w", err)
	}

	return link != nil, nil
}

// childSnapshot builds a childResult from the durable link state (authoritative
// terminal signal) plus the child's iteration count and final output.
func (s *svc) childSnapshot(ctx context.Context, childID int64) (childResult, error) {
	link, err := s.links.GetLink(ctx, childID)
	if err != nil {
		return childResult{}, fmt.Errorf("get link: %w", err)
	}

	if link == nil {
		return childResult{}, fmt.Errorf("subagent %d not found", childID)
	}

	res := childResult{
		ChildID:  childID,
		State:    link.State,
		Terminal: link.Terminal(),
	}

	if rec, rerr := s.sessionStore.GetSession(ctx, childID); rerr == nil {
		res.Iteration = rec.Iteration
	}

	// Read the stored result/outcome (written at terminalization) only once
	// terminal — a re-engaged child clears delivered_at but leaves result/outcome
	// stale until its next terminalization, so guarding on Terminal() hides it.
	if res.Terminal {
		res.Output = link.Result
		res.Outcome = link.Outcome
	}

	return res, nil
}

// childDepth returns the depth a new child of parentID should have: one deeper
// than the parent's own link (root parents have no link → child depth 1). A read
// failure cancels the spawn rather than silently resetting the nesting cap.
func (s *svc) childDepth(ctx context.Context, parentID int64) (int, error) {
	link, err := s.links.GetLink(ctx, parentID)
	if err != nil {
		return 0, fmt.Errorf("parent link %d: %w", parentID, err)
	}

	if link == nil {
		return 1, nil
	}

	return link.Depth + 1, nil
}

// resolveChildEffort settles the child's level against the CHILD's model, since
// that is the only vocabulary its run is measured against. An asked-for level the
// model rejects fails the spawn; a merely inherited one falls back to its default.
func (s *svc) resolveChildEffort(model, requested, inherited string) (string, error) {
	if len(s.modelEntries) == 0 {
		return cmp.Or(requested, inherited), nil
	}

	if requested != "" {
		level, err := session.ResolveReasoningLevel(s.modelEntries, model, requested)
		if err != nil {
			return "", fmt.Errorf("spawn subagent on model %s: %w", model, err)
		}

		return level, nil
	}

	if level, err := session.ResolveReasoningLevel(s.modelEntries, model, inherited); err == nil {
		return level, nil
	}

	level, err := session.ResolveReasoningLevel(s.modelEntries, model, "")
	if err != nil {
		return "", fmt.Errorf("spawn subagent on model %s: %w", model, err)
	}

	return level, nil
}

// resolveChildModel picks the child model: explicit request → agent type override → parent model.
func (s *svc) resolveChildModel(req spawnRequest, parentRec *sessionstore.SessionRecord) string {
	switch {
	case req.Model != "":
		return req.Model
	case req.AgentModel != "":
		return req.AgentModel
	default:
		return parentRec.Model
	}
}
