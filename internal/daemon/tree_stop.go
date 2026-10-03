package daemon

import (
	"context"
	"fmt"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

type stopPlan struct {
	rootID     int64
	sessionIDs []int64
	links      []subagent.Link
}

func (p *stopPlan) SessionIDs() []int64 {
	return append([]int64(nil), p.sessionIDs...)
}

func (s *svc) beginStop(ctx context.Context, rootID int64, liveSessionIDs []int64) (*stopPlan, error) {
	plan, err := s.stopPlan(ctx, rootID, liveSessionIDs)
	if err != nil {
		return nil, err
	}

	for _, id := range plan.sessionIDs {
		if err := s.store.UpdateSessionStatus(ctx, id, sessionstore.SessionStatusStopping); err != nil {
			return nil, fmt.Errorf("mark session %d stopping: %w", id, err)
		}
	}

	for _, link := range plan.links {
		if err := s.links.MarkLinkStopped(ctx, link.ChildID); err != nil {
			return nil, fmt.Errorf("mark subagent %d stopped: %w", link.ChildID, err)
		}
	}

	return plan, nil
}

func (s *svc) cancelStopInputs(ctx context.Context, plan *stopPlan) error {
	if _, err := s.store.CancelPendingInputsForStop(ctx, plan.sessionIDs, "stopped"); err != nil {
		return fmt.Errorf("cancel stopped session input: %w", err)
	}

	return nil
}

func (s *svc) finishStop(ctx context.Context, plan *stopPlan, keepRootStopping bool) error {
	for _, link := range plan.links {
		if err := s.links.MakeStoppedLinkResumable(ctx, link.ChildID); err != nil {
			return fmt.Errorf("detach stopped subagent %d: %w", link.ChildID, err)
		}
	}

	for _, id := range plan.sessionIDs {
		if keepRootStopping && id == plan.rootID {
			continue
		}

		if err := s.store.UpdateSessionStatus(ctx, id, sessionstore.SessionStatusStopped); err != nil {
			return fmt.Errorf("mark session %d stopped: %w", id, err)
		}
	}

	return nil
}

func (s *svc) completeExplicitStop(
	ctx context.Context,
	rootID, inputID int64,
	cancelledProcesses int,
) error {
	if _, err := s.store.CompleteExplicitStop(ctx, rootID, inputID, cancelledProcesses); err != nil {
		return fmt.Errorf("commit explicit stop completion: %w", err)
	}

	if _, err := s.store.ReactivateForSchedule(ctx, rootID); err != nil {
		return fmt.Errorf("complete explicit stop: %w", err)
	}

	record, err := s.store.GetSession(ctx, rootID)
	if err != nil {
		return nil //nolint:nilerr // The committed terminal fact is authoritative; wake is best-effort.
	}

	owner, _ := record.Attributes[controllerapi.SessionAttributeManagerID].(string)
	if owner != "" {
		_, _ = s.store.WakeOutputHead(ctx, owner)
	}

	return nil
}

//nolint:wsl_v5 // Persisted tree membership and live-runner repair form one plan.
func (s *svc) stopPlan(ctx context.Context, rootID int64, liveSessionIDs []int64) (*stopPlan, error) {
	records, err := s.store.ListAllSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list sessions for stop tree: %w", err)
	}

	byParent := make(map[int64][]*sessionstore.SessionRecord)
	byID := make(map[int64]*sessionstore.SessionRecord, len(records))
	foundRoot := false

	for _, record := range records {
		byID[record.ID] = record
		if record.ID == rootID {
			foundRoot = true
		}

		if record.ParentID != 0 && record.KilledAt == nil {
			byParent[record.ParentID] = append(byParent[record.ParentID], record)
		}
	}

	if !foundRoot {
		return nil, fmt.Errorf("session %d not found", rootID)
	}

	ids := activeTreeIDs(rootID, byParent)
	included := make(map[int64]bool, len(ids)+len(liveSessionIDs))
	for _, id := range ids {
		included[id] = true
	}
	for _, id := range liveSessionIDs {
		record := byID[id]
		if record == nil || record.KilledAt != nil ||
			(record.ID != rootID && record.RootID != rootID) || included[id] {
			continue
		}
		ids = append(ids, id)
		included[id] = true
	}

	links := make([]subagent.Link, 0, len(ids))
	for _, id := range ids {
		link, linkErr := s.links.GetLink(ctx, id)
		if linkErr != nil {
			return nil, fmt.Errorf("load subagent link for session %d: %w", id, linkErr)
		}

		if link != nil && !link.Terminal() {
			links = append(links, *link)
		}
	}

	return &stopPlan{rootID: rootID, sessionIDs: ids, links: links}, nil
}

func activeTreeIDs(rootID int64, byParent map[int64][]*sessionstore.SessionRecord) []int64 {
	ids := []int64{rootID}
	walk := []int64{rootID}
	seen := map[int64]bool{rootID: true}

	for pos := 0; pos < len(walk); pos++ {
		for _, child := range byParent[walk[pos]] {
			if seen[child.ID] {
				continue
			}

			seen[child.ID] = true

			walk = append(walk, child.ID)
			if stopActive(child.Status) {
				ids = append(ids, child.ID)
			}
		}
	}

	return ids
}

func stopActive(status sessionstore.SessionStatus) bool {
	return status == sessionstore.SessionStatusActive ||
		status == sessionstore.SessionStatusSuspended ||
		status == sessionstore.SessionStatusStopping ||
		status == sessionstore.SessionStatusStopped
}
