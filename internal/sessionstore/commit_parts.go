package sessionstore

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

func prepareCommitTx(ctx context.Context, tx *sql.Tx, c Commit) (Commit, bool, error) {
	if c.At.IsZero() {
		c.At = time.Now().UTC()
	}
	if c.RootID == 0 {
		c.RootID = c.SessionID
	}
	if err := checkCommitFence(ctx, tx, c); err != nil {
		return c, false, err
	}
	replay, err := acceptedBatchReplay(ctx, tx, c)
	if err != nil {
		return c, false, err
	}
	if replay {
		if err := prepareReplayOutputKeys(ctx, tx, &c); err != nil {
			return c, false, err
		}
	}
	return c, replay, nil
}

func applyCommitTx(ctx context.Context, tx *sql.Tx, c Commit, replay bool, result *CommitResult) error {
	if err := commitBoundaryPartsTx(ctx, tx, c, replay, result); err != nil {
		return err
	}
	toolIDs, err := commitToolResultsTx(ctx, tx, c)
	if err != nil {
		return err
	}
	if err := commitReplaceTx(ctx, tx, c); err != nil {
		return err
	}
	if c.ObserveBudget {
		if err := observeCommitBudgetTx(ctx, tx, c, result); err != nil {
			return err
		}
	}
	if err := commitUnfiredPartsTx(ctx, tx, c, result); err != nil {
		return err
	}
	result.MessageIDs = append(result.MessageIDs, toolIDs...)
	if err := applyStatePatchTx(ctx, tx, c.SessionID, c.State, result.MessageIDs, c.At); err != nil {
		return err
	}
	return commitOutputPartsTx(ctx, tx, c, result)
}

func commitBoundaryPartsTx(ctx context.Context, tx *sql.Tx, c Commit, replay bool, result *CommitResult) error {
	linked, err := commitAcceptPartsTx(ctx, tx, c, replay, result)
	if err != nil {
		return err
	}
	deferred := c.Activation != nil && activationLinked(c.Activation, linked)
	if !deferred {
		if err := commitActivationPartTx(ctx, tx, c, result); err != nil {
			return err
		}
	}
	if err := commitMessagePartsTx(ctx, tx, c, linked, replay, result); err != nil {
		return err
	}
	if deferred {
		return commitActivationPartTx(ctx, tx, c, result)
	}
	return nil
}

func commitAcceptPartsTx(ctx context.Context, tx *sql.Tx, c Commit, replay bool, result *CommitResult) ([]Accept, error) {
	var linked []Accept
	for _, accept := range c.Accept {
		if !replay && accept.State == InputStateAccepted && accept.Content == "" && accept.LinkRef >= 0 {
			linked = append(linked, accept)
			continue
		}
		if err := commitAcceptTx(ctx, tx, c, accept, 0, result); err != nil {
			return nil, err
		}
	}
	return linked, nil
}

func commitActivationPartTx(ctx context.Context, tx *sql.Tx, c Commit, result *CommitResult) error {
	if c.Activation == nil {
		return nil
	}
	grant, err := commitActivationTx(ctx, tx, c)
	if err != nil {
		return err
	}
	result.Activation = grant
	return nil
}

func commitMessagePartsTx(ctx context.Context, tx *sql.Tx, c Commit, linked []Accept, replay bool, result *CommitResult) error {
	if replay {
		return nil
	}
	if c.State.ResetContext {
		if err := resetContextTx(ctx, tx, c.SessionID, c.At); err != nil {
			return err
		}
	}
	offset := len(result.MessageIDs)
	if err := appendReferencedMessages(ctx, tx, c, linked, result); err != nil {
		return err
	}
	for _, accept := range linked {
		if accept.LinkRef >= len(c.Messages) {
			return errors.New("input message reference out of range")
		}
		if err := commitAcceptTx(ctx, tx, c, accept, result.MessageIDs[offset+accept.LinkRef], result); err != nil {
			return err
		}
	}
	return nil
}

func commitToolResultsTx(ctx context.Context, tx *sql.Tx, c Commit) ([]int64, error) {
	var ids []int64
	for _, message := range c.ToolResults {
		id, fresh, err := insertToolResultOnceAt(ctx, tx, c.SessionID, message, c.At)
		if err != nil {
			return nil, err
		}
		if fresh {
			if err := invalidateCompletionCheckTx(ctx, tx, c.SessionID); err != nil {
				return nil, err
			}
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func commitReplaceTx(ctx context.Context, tx *sql.Tx, c Commit) error {
	if c.Replace == nil {
		return nil
	}
	_, err := replaceCompactedMessagesTx(ctx, tx, c.SessionID, c.Replace.HeadIDs, c.Replace.Entries, c.At)
	return err
}

func commitUnfiredPartsTx(ctx context.Context, tx *sql.Tx, c Commit, result *CommitResult) error {
	if result.BudgetFired {
		return nil
	}
	if err := appendCommitMessages(ctx, tx, c.SessionID, c.Unfired.Messages, result); err != nil {
		return err
	}
	return applyStatePatchTx(ctx, tx, c.SessionID, c.Unfired.State, result.MessageIDs, c.At)
}

func commitOutputPartsTx(ctx context.Context, tx *sql.Tx, c Commit, result *CommitResult) error {
	if !result.BudgetFired {
		if err := commitOutputsTx(ctx, tx, c, c.Unfired.Outputs, result); err != nil {
			return err
		}
	}
	return commitOutputsTx(ctx, tx, c, c.Outputs, result)
}
