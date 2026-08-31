package sessionlifecycle

import (
	"context"
	"fmt"
	"sync"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

type StopPlan struct {
	rootID     int64
	sessionIDs []int64
	links      []subagent.Link
}

func (p *StopPlan) SessionIDs() []int64 {
	return append([]int64(nil), p.sessionIDs...)
}

type Stopper interface {
	GuardSpawn(run func() error) error
	Begin(ctx context.Context, rootID int64) (*StopPlan, error)
	CancelInputs(ctx context.Context, plan *StopPlan) error
	Finish(ctx context.Context, plan *StopPlan, keepRootStopping bool) error
	CompleteExplicit(ctx context.Context, rootID, inputID int64) error
	InterruptedExplicitStops(ctx context.Context) ([]sessionstore.InterruptedExplicitStop, error)
}

var _ Stopper = (*stopper)(nil)

type stopper struct {
	mu        sync.Locker
	sessions  sessionstore.OrchestrationStore
	lifecycle sessionstore.SessionLifecycleStore
	outputs   sessionstore.ManagerOutputStore
	links     subagent.Store
}

func NewStopper(
	sessions sessionstore.OrchestrationStore,
	lifecycle sessionstore.SessionLifecycleStore,
	outputs sessionstore.ManagerOutputStore,
	links subagent.Store,
) Stopper {
	return NewStopperWithLock(sessions, lifecycle, outputs, links, &sync.Mutex{})
}

func NewStopperWithLock(
	sessions sessionstore.OrchestrationStore,
	lifecycle sessionstore.SessionLifecycleStore,
	outputs sessionstore.ManagerOutputStore,
	links subagent.Store,
	lock sync.Locker,
) Stopper {
	return &stopper{
		mu: lock, sessions: sessions, lifecycle: lifecycle, outputs: outputs, links: links,
	}
}

func (s *stopper) GuardSpawn(run func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return run()
}

func (s *stopper) Begin(ctx context.Context, rootID int64) (*StopPlan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	plan, err := s.stopPlan(ctx, rootID)
	if err != nil {
		return nil, err
	}

	for _, id := range plan.sessionIDs {
		if err := s.sessions.UpdateSessionStatus(ctx, id, sessionstore.SessionStatusStopping); err != nil {
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

func (s *stopper) CancelInputs(ctx context.Context, plan *StopPlan) error {
	if _, err := s.lifecycle.CancelPendingInputs(ctx, plan.sessionIDs, "stopped"); err != nil {
		return fmt.Errorf("cancel stopped session input: %w", err)
	}

	return nil
}

func (s *stopper) Finish(ctx context.Context, plan *StopPlan, keepRootStopping bool) error {
	for _, link := range plan.links {
		if err := s.links.MakeStoppedLinkResumable(ctx, link.ChildID); err != nil {
			return fmt.Errorf("detach stopped subagent %d: %w", link.ChildID, err)
		}
	}

	for _, id := range plan.sessionIDs {
		if keepRootStopping && id == plan.rootID {
			continue
		}

		if err := s.sessions.UpdateSessionStatus(ctx, id, sessionstore.SessionStatusStopped); err != nil {
			return fmt.Errorf("mark session %d stopped: %w", id, err)
		}
	}

	return nil
}

func (s *stopper) CompleteExplicit(ctx context.Context, rootID, inputID int64) error {
	if _, err := s.lifecycle.CompleteExplicitStop(ctx, rootID, inputID); err != nil {
		return fmt.Errorf("commit explicit stop completion: %w", err)
	}

	record, err := s.sessions.GetSession(ctx, rootID)
	if err != nil {
		return nil //nolint:nilerr // The committed terminal fact is authoritative; wake is best-effort.
	}

	owner, _ := record.Attributes[controllerapi.SessionAttributeManagerID].(string)
	if s.outputs != nil && owner != "" {
		_, _ = s.outputs.WakeOutputHead(ctx, owner)
	}

	return nil
}

func (s *stopper) InterruptedExplicitStops(
	ctx context.Context,
) ([]sessionstore.InterruptedExplicitStop, error) {
	stops, err := s.lifecycle.SelectInterruptedExplicitStops(ctx)
	if err != nil {
		return nil, fmt.Errorf("select interrupted explicit stops: %w", err)
	}

	return stops, nil
}

func (s *stopper) stopPlan(ctx context.Context, rootID int64) (*StopPlan, error) {
	records, err := s.sessions.ListAllSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list sessions for stop tree: %w", err)
	}

	byParent := make(map[int64][]*sessionstore.SessionRecord)
	foundRoot := false

	for _, record := range records {
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

	links := make([]subagent.Link, 0, len(ids))
	for _, id := range ids {
		link, linkErr := s.links.GetLink(ctx, id)
		if linkErr != nil {
			return nil, fmt.Errorf("load subagent link for session %d: %w", id, linkErr)
		}

		if link != nil && !link.Terminal() && link.State != subagent.StateStopped {
			links = append(links, *link)
		}
	}

	return &StopPlan{rootID: rootID, sessionIDs: ids, links: links}, nil
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
		status == sessionstore.SessionStatusStopping
}
