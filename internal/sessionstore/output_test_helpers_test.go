package sessionstore

import (
	"context"

	"github.com/pilat/coagent/internal/transcript"
)

func insertSingleToolResult(
	ctx context.Context,
	store DirectOutputStore,
	sessionID int64,
	message *transcript.Message,
	directMessages []string,
) (int64, []*OutputCommit, error) {
	ids, outputs, err := store.InsertToolResultSetOnce(ctx, sessionID, []ToolResultEntry{
		{Message: message, DirectMessages: directMessages},
	})
	if err != nil {
		return 0, nil, err
	}

	return ids[0], outputs[0], nil
}
