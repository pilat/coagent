package backgroundprocess

import (
	"context"

	"github.com/pilat/coagent/internal/sessionstore"
)

func processInputs(ctx context.Context, ledger Store, sessionID int64) ([]*sessionstore.InboxInput, error) {
	return ledger.(*store).sessions.ListPending(ctx, sessionID)
}
