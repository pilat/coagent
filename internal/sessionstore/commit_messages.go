package sessionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

func acceptedBatchReplay(ctx context.Context, tx *sql.Tx, c Commit) (bool, error) {
	var replayed, pending int

	for _, accept := range c.Accept {
		if accept.State != InputStateAccepted {
			continue
		}

		input, err := loadInboxInput(ctx, tx, accept.InputID)
		if err != nil {
			return false, err
		}

		if input.SessionID != c.SessionID {
			return false, ErrInputNotFound
		}

		switch input.State {
		case InputStateAccepted:
			replayed++
		case InputStatePending:
			pending++
		case InputStateHandled, InputStateRejected, InputStateCancelled:
			return false, ErrInputResolved
		}
	}

	if replayed != 0 && pending != 0 {
		return false, errors.New("accepted input replay cannot mix with new accepted input")
	}

	return replayed != 0, nil
}

func prepareReplayOutputKeys(ctx context.Context, tx *sql.Tx, c *Commit) error {
	refs := make(map[int]int64)
	offset := 0

	for _, accept := range c.Accept {
		if accept.State == InputStateAccepted && (accept.Content != "" || accept.LinkRef < 0) {
			offset++
		}
	}

	direct := 0

	for _, accept := range c.Accept {
		if accept.State != InputStateAccepted {
			continue
		}

		input, err := loadInboxInput(ctx, tx, accept.InputID)
		if err != nil {
			return err
		}

		ref := offset + accept.LinkRef
		if accept.Content != "" || accept.LinkRef < 0 {
			ref = direct
			direct++
		}

		refs[ref] = input.AcceptedMessageID
	}
	var err error

	c.Outputs, err = replayOutputKeys(c.Outputs, refs)
	if err != nil {
		return err
	}

	c.Unfired.Outputs, err = replayOutputKeys(c.Unfired.Outputs, refs)

	return err
}

func replayOutputKeys(outputs []Output, refs map[int]int64) ([]Output, error) {
	out := append([]Output(nil), outputs...)
	for index, output := range out {
		if output.Key != "" || output.Phase == "" {
			continue
		}

		id, ok := refs[output.MessageRef]
		if !ok {
			return nil, errors.New("accepted input replay output references an unrecorded message")
		}

		out[index].Key = fmt.Sprintf("message:%d:%s", id, output.Phase)
	}

	return out, nil
}

func appendReferencedMessages(ctx context.Context, tx *sql.Tx, c Commit, linked []Accept, result *CommitResult) error {
	existing := make(map[int]int64)

	for _, accept := range linked {
		if accept.LinkRef < 0 || accept.LinkRef >= len(c.Messages) {
			return errors.New("input message reference out of range")
		}

		input, err := loadInboxInput(ctx, tx, accept.InputID)
		if err != nil {
			return err
		}

		if input.SessionID != c.SessionID {
			return ErrInputNotFound
		}

		if input.State == InputStateAccepted {
			existing[accept.LinkRef] = input.AcceptedMessageID
		}
	}

	for index := range c.Messages {
		if id := existing[index]; id != 0 {
			result.MessageIDs = append(result.MessageIDs, id)
			continue
		}

		if err := appendCommitMessages(ctx, tx, c.SessionID, c.Messages[index:index+1], result); err != nil {
			return err
		}
	}

	return nil
}
