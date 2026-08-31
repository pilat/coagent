package daemon

import (
	"context"
	"fmt"

	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

func (s *svc) stopTree(ctx context.Context, rootID int64) ([]int64, []subagent.Link, error) {
	records, err := s.sessionStore.ListAllSessions(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list sessions for stop tree: %w", err)
	}

	byParent := make(map[int64][]*sessionstore.SessionRecord)
	foundRoot := false

	for _, rec := range records {
		if rec.ID == rootID {
			foundRoot = true
		}

		if rec.ParentID != 0 && rec.KilledAt == nil {
			byParent[rec.ParentID] = append(byParent[rec.ParentID], rec)
		}
	}

	if !foundRoot {
		return nil, nil, fmt.Errorf("session %d not found", rootID)
	}

	ids := []int64{rootID}
	walk := []int64{rootID}
	seen := map[int64]bool{rootID: true}
	var treeLinks []subagent.Link

	for pos := 0; pos < len(walk); pos++ {
		for _, child := range byParent[walk[pos]] {
			if seen[child.ID] {
				continue
			}

			seen[child.ID] = true
			walk = append(walk, child.ID)

			if isStopActive(child.Status) {
				ids = append(ids, child.ID)
			}
		}
	}

	// Include the requested session's own link when /stop targets a child, and
	// include every active descendant link even below a terminal ancestor.
	for _, id := range ids {
		link, linkErr := s.links.GetLink(ctx, id)
		if linkErr != nil {
			return nil, nil, fmt.Errorf("load subagent link for session %d: %w", id, linkErr)
		}

		if link != nil && !link.Terminal() && link.State != subagent.StateStopped {
			treeLinks = append(treeLinks, *link)
		}
	}

	return ids, treeLinks, nil
}

func isStopActive(status sessionstore.SessionStatus) bool {
	return status == sessionstore.SessionStatusActive ||
		status == sessionstore.SessionStatusSuspended ||
		status == sessionstore.SessionStatusStopping
}

func (s *svc) removeQueuedSessions(ids []int64) {
	stopped := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		stopped[id] = struct{}{}
	}

	s.childQueue.Remove(func(queued queuedChild) bool {
		_, remove := stopped[queued.sessionID]

		return remove
	})
	s.pendingQueue.Remove(func(queued queuedRunner) bool {
		_, remove := stopped[queued.sessionID]

		return remove
	})
}
