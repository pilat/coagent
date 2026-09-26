package daemon

import (
	"context"
	"fmt"
)

func (s *svc) retireShieldPolicy(ctx context.Context, rootID int64) error {
	if s.networkOwner != nil {
		if err := s.networkOwner.Retire(ctx, rootID); err != nil {
			return fmt.Errorf("retire shielded tree network: %w", err)
		}
	}

	return nil
}
