package sessionstore

import (
	"context"
	"database/sql"
)

func appendReferencedMessages(ctx context.Context, tx *sql.Tx, c Commit, result *CommitResult) error {
	return appendCommitMessages(ctx, tx, c.SessionID, c.Messages, result)
}
