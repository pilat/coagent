package progressruntime

import (
	"context"
	"errors"
	"fmt"

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
		return nil, false, fmt.Errorf("load progress output: %w", err)
	}

	return record, true, nil
}
