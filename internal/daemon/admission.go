package daemon

import (
	"context"
	"fmt"

	"github.com/pilat/coagent/internal/subagent"
)

func (s *svc) drainQueue(ctx context.Context) { s.supervisor.DrainChildren(ctx) }

// childTerminated reports whether a queued child was killed/terminalized before it
// got a runner (e.g. by cascadeKillChildren). Checked just before launch so a
// stale queue entry is never turned into a live runner. An unreadable ledger is
// neither answer, so the caller must defer the decision instead of guessing.
func (s *svc) childTerminated(ctx context.Context, childID int64) (bool, error) {
	link, err := s.links.GetLink(ctx, childID)
	if err != nil {
		return false, fmt.Errorf("queued child link %d: %w", childID, err)
	}

	if link != nil && (link.Terminal() || link.State == subagent.StateStopped) {
		return true, nil
	}

	rec, err := s.sessionStore.GetSession(ctx, childID)
	if err != nil {
		return false, fmt.Errorf("queued child session %d: %w", childID, err)
	}

	return rec.KilledAt != nil, nil
}
