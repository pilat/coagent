package daemon

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/subagent"
)

func (s *svc) cascadeKillChildrenForKilledTree(
	ctx context.Context,
	parentID int64,
	depth int,
	deadline time.Time,
) {
	s.cascadeKillChildrenMode(ctx, parentID, depth, deadline, true)
}

func (s *svc) cascadeKillChildrenMode(
	ctx context.Context,
	parentID int64,
	depth int,
	deadline time.Time,
	suppressTerminal bool,
) {
	if depth >= maxDepth {
		return
	}

	// Pending links include terminal-undelivered and queued children; the
	// terminal guard preserves the former's stored outcome.
	links, err := s.links.ListPendingChildLinks(ctx, parentID)
	if err != nil {
		// The walk stops here, so part of the subtree survives the teardown.
		logger.Ctx(ctx).Named("daemon.completion").
			Error("cascade_list_children", zap.Int64("parent", parentID), zap.Error(err))

		return
	}

	for _, link := range links {
		if link.Terminal() {
			if suppressTerminal && !link.Blocking {
				s.deliverBackgroundCompletion(ctx, link)
			}

			continue // already done (e.g. completed-but-undelivered) — keep its result
		}

		s.cascadeKillChildrenMode(ctx, link.ChildID, depth+1, deadline, suppressTerminal)
		s.warnKilledDescendant(ctx, link)
		s.killSubagent(ctx, link.ChildID, deadline)
	}
}

// warnKilledDescendant emits one audit line per non-terminal descendant torn down
// by a cascade kill, using only fields already in hand (no message load).
func (s *svc) warnKilledDescendant(ctx context.Context, link subagent.Link) {
	iteration := 0
	if rec, err := s.store.GetSession(ctx, link.ChildID); err == nil {
		iteration = rec.Iteration
	}

	logger.Ctx(ctx).Named("daemon.completion").Warn(
		"cascade_killed_descendant",
		zap.Int64("child", link.ChildID),
		zap.Int64("parent", link.ParentID),
		zap.String("state", string(link.State)),
		zap.Int("iteration", iteration),
	)
}

// killSubagent marks a child killed durably, then stops its runner. The terminal
// mark precedes the stop so the child's own loop teardown (finalizeChild) observes
// a terminal link and no-ops, rather than racing to deliver a stray completion.
// deadline bounds the terminal-mark retry across the whole cascade (zero = unbounded).
func (s *svc) killSubagent(ctx context.Context, childID int64, deadline time.Time) {
	if err := s.retireTreeToolResources(ctx, childID); err != nil {
		logger.Ctx(ctx).Named("daemon.completion").Warn("retire_child_tools", zap.Error(err))
	}
	// Link-terminal must commit before the status write: it is the authoritative
	// sweep signal, and MarkSessionKilled below hides a non-terminal link for good.
	err := s.markLinkTerminalRetrying(ctx, deadline, childID, subagent.StateKilled, "", subagent.OutcomeKilled)
	if err != nil {
		// Skipping killed_at is what keeps the child recoverable: the sweep selects
		// on `state IN ('spawned','running') AND killed_at IS NULL`.
		logger.Ctx(ctx).
			Named("daemon.completion").
			Error("kill_link_terminal", zap.Int64("child", childID), zap.Error(err))
	}

	if err == nil {
		s.markChildKilled(ctx, childID)
	}

	s.removeSchedules(ctx, childID)

	rs, ok := s.runners.load(childID)

	if ok {
		rs.Stop()
	}
}

// markChildKilled writes the session half of a kill. Called only once the link is
// terminal — the two writes together are what make the child invisible to the sweep.
func (s *svc) markChildKilled(ctx context.Context, childID int64) {
	if err := s.store.MarkSessionKilled(ctx, childID); err != nil {
		logger.Ctx(ctx).
			Named("daemon.completion").
			Error("kill_child_session", zap.Int64("child", childID), zap.Error(err))
	}
}
