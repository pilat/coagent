//nolint:wrapcheck // Store sentinels are part of the reconciliation contract.
package progressruntime

import (
	"context"
	"errors"

	"github.com/pilat/coagent/internal/sessionstore"
)

func (r *runtime) existingProgressOutput(
	ctx context.Context,
	rootID int64,
	sourceKey string,
) (*sessionstore.OutputRecord, bool, error) {
	record, err := r.sessionStore.OutputBySourceKey(ctx, rootID, sourceKey)

	if errors.Is(err, sessionstore.ErrNoOutput) {
		return nil, false, nil
	}

	if err != nil {
		return nil, false, err
	}

	return record, true, nil
}
