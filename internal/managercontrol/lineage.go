package managercontrol

import (
	"context"
	"fmt"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionstore"
)

var _ controllerapi.LineageController = (*controller)(nil)

func (c *controller) Successor(ctx context.Context, sessionID int64) (int64, error) {
	record, err := c.chatSession(ctx, sessionID)
	if err != nil {
		return 0, err
	}

	if record.Status != sessionstore.SessionStatusTerminating && record.KilledAt == nil {
		return 0, nil
	}

	chain, err := c.app.store.ReplacementChain(ctx, record.ID)
	if err != nil {
		return 0, fmt.Errorf("read root replacement chain: %w", err)
	}

	for i, id := range chain {
		if id == record.ID && i+1 < len(chain) {
			return chain[i+1], nil
		}
	}

	return 0, nil
}
