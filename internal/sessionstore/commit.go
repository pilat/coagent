package sessionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/progress"
	"github.com/pilat/coagent/internal/transcript"
)

// Commit preserves step ordering and fences live-loop writes against lifecycle settlement.
func (s *Store) Commit(ctx context.Context, c Commit) (*CommitResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin session commit: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	c, replay, err := prepareCommitTx(ctx, tx, c)
	if err != nil {
		return nil, err
	}

	result := &CommitResult{}
	if err := applyCommitTx(ctx, tx, c, replay, result); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit session step: %w", err)
	}

	s.recordWoken(c.SessionID)

	if result.BudgetFired && c.RootID != c.SessionID {
		s.recordWoken(c.RootID)
	}

	return result, nil
}

func activationLinked(change *ActivationChange, linked []Accept) bool {
	for _, accept := range linked {
		if accept.InputID == change.InputID {
			return true
		}
	}

	return false
}

func appendCommitMessages(
	ctx context.Context,
	tx *sql.Tx,
	sessionID int64,
	messages []*transcript.Message,
	result *CommitResult,
) error {
	for _, message := range messages {
		if message == nil {
			return errors.New("nil commit message")
		}

		stored := *message
		if stored.RetryOfRef != nil {
			ref := *stored.RetryOfRef
			if ref < 0 || ref >= len(result.MessageIDs) {
				return errors.New("retry message reference out of range")
			}

			stored.RetryOfMessageID = result.MessageIDs[ref]
		}

		id, err := insertMessageWith(ctx, tx, sessionID, &stored)
		if err != nil {
			return err
		}

		result.MessageIDs = append(result.MessageIDs, id)
	}

	return nil
}

func commitOutputsTx(ctx context.Context, tx *sql.Tx, c Commit, outputs []Output, result *CommitResult) error {
	for _, output := range outputs {
		target := output.Session
		if target == 0 {
			target = c.SessionID
		}

		_, err := outputOwner(ctx, tx, target)
		if errors.Is(err, ErrOutputOwner) || errors.Is(err, ErrOutputNotRoot) {
			continue
		}

		if err != nil {
			return err
		}

		key := output.Key
		if key == "" && output.Phase != "" {
			if output.MessageRef < 0 || output.MessageRef >= len(result.MessageIDs) {
				return errors.New("output message reference out of range")
			}

			key = fmt.Sprintf("message:%d:%s", result.MessageIDs[output.MessageRef], output.Phase)
		}

		content := output.Content
		if output.FinalFooter != nil {
			facts, err := CaptureProgressTx(ctx, tx, target)
			if err != nil {
				return err
			}

			final, err := finalFactsFromProgress(facts, c.At)
			if err != nil {
				return err
			}

			final.IsBackgroundYield = output.FinalFooter.BackgroundYield
			content = progress.RenderFinalFromFacts(content, final)
		}

		out, err := insertOutputTx(ctx, tx, OutputDraft{
			SessionID: target, Type: output.Type, Content: content,
			Attributes: output.Attributes, SourceKey: key, ReleasesInput: output.ReleasesInput, CreatedAt: c.At,
		}, c.Mode)
		if err != nil {
			return err
		}

		if out != nil {
			out.LiveContent = output.Content
			result.Outputs = append(result.Outputs, out)
		}
	}

	return nil
}
