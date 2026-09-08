package daemon

import (
	"context"
	"fmt"

	"github.com/pilat/coagent/internal/backgroundprocess"
)

func (s *svc) cancelSessionSubtreeProcesses(
	ctx context.Context,
	sessionID int64,
	intent backgroundprocess.HostIntent,
) (int, error) {
	if s.processSvc == nil {
		return 0, nil
	}

	sessionIDs, err := s.sessionSubtreeIDs(ctx, sessionID)
	if err != nil {
		return 0, err
	}

	cancelled, err := s.processSvc.CancelSessions(ctx, sessionIDs, intent)
	if err != nil {
		return cancelled, fmt.Errorf("cancel subtree processes: %w", err)
	}

	return cancelled, nil
}

func (s *svc) sessionSubtreeIDs(ctx context.Context, sessionID int64) ([]int64, error) {
	records, err := s.sessionStore.ListAllSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list process owner sessions: %w", err)
	}

	children := make(map[int64][]int64)
	found := false

	for _, record := range records {
		if record.ID == sessionID {
			found = true
		}

		if record.ParentID != 0 {
			children[record.ParentID] = append(children[record.ParentID], record.ID)
		}
	}

	if !found {
		return nil, fmt.Errorf("session %d not found", sessionID)
	}

	return walkSessionSubtree(sessionID, children), nil
}

func walkSessionSubtree(rootID int64, children map[int64][]int64) []int64 {
	ids := []int64{rootID}
	seen := map[int64]bool{rootID: true}

	for pos := 0; pos < len(ids); pos++ {
		for _, childID := range children[ids[pos]] {
			if seen[childID] {
				continue
			}

			seen[childID] = true
			ids = append(ids, childID)
		}
	}

	return ids
}
