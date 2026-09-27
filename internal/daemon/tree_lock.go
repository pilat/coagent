package daemon

import (
	"context"
	"fmt"
)

func (s *svc) lockSessionTree(ctx context.Context, sessionID int64) (func(), error) {
	unlock, err := s.supervisor.LockTree(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("lock session tree: %w", err)
	}

	return unlock, nil
}
