package assistant

import (
	"context"
	"database/sql"
	"fmt"
	"slices"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionstore"
)

const dialogTailLines = 6

func (l *loop) commitStep(ctx context.Context, s step) error {
	err := l.deps.Store.WithTx(ctx, func(tx *sql.Tx) error {
		now := l.deps.Now()
		if err := SaveDeskTx(ctx, tx, l.sessionID, Desk{
			Diary: s.answer.Diary, Stickers: s.answer.Stickers,
			Focus: s.answer.Focus, LastTurnAt: now, UpdatedAt: now,
		}); err != nil {
			return err
		}

		if err := l.commitTape(ctx, tx, s); err != nil {
			return err
		}

		if err := RecordTurnTx(ctx, tx, Turn{
			SessionID: l.sessionID, InputID: s.inputID, Step: s.number,
			Desk: s.render, Response: s.raw, Outcome: "ok", CreatedAt: now,
		}); err != nil {
			return err
		}

		if err := sessionstore.HandleAssistantInputTx(
			ctx,
			tx,
			l.sessionID,
			s.inputID,
			"assistant_turn",
			now,
		); err != nil {
			return fmt.Errorf("commit assistant input: %w", err)
		}

		if s.answer.Say != nil {
			return l.deps.SayTx(ctx, tx, *s.answer.Say)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("commit assistant step: %w", err)
	}

	if s.answer.Say != nil {
		l.deps.Notify()
	}

	return nil
}

func (l *loop) commitTape(ctx context.Context, tx *sql.Tx, s step) error {
	var lines []string

	if s.answer.Say != nil {
		if _, err := AppendTapeTx(ctx, tx, l.sessionID,
			Line{At: l.deps.Now(), Kind: sayField, Text: "say: «" + *s.answer.Say + "»"}); err != nil {
			return err
		}
	}

	for _, action := range s.answer.Do {
		if action.Send != nil || action.Stop != nil {
			lines = append(lines, actionLine(action, s.board))
		}
	}

	for _, text := range lines {
		if _, err := AppendTapeTx(ctx, tx, l.sessionID, Line{At: l.deps.Now(), Kind: "move", Text: text}); err != nil {
			return err
		}
	}

	return HideTapeTx(ctx, tx, l.sessionID, forgetRanges(s))
}

// forgetRanges never reaches lines the model has not seen, and keeps the turn's
// own lines visible while a read continues the turn.
func forgetRanges(s step) [][2]int64 {
	seen := s.event.LineID
	for _, line := range s.tape {
		seen = max(seen, line.ID)
	}

	keep := dialogTail(s.tape, dialogTailLines)
	if slices.ContainsFunc(s.answer.Do, func(a Action) bool { return a.Read != nil }) {
		keep = append(keep, s.origin, s.event.LineID)
	}

	ranges := make([][2]int64, 0, len(s.answer.Forget))
	for _, span := range s.answer.Forget {
		ranges = append(ranges, splitAround([2]int64{span[0], min(span[1], seen)}, keep)...)
	}

	return ranges
}

// dialogTail is the last few user and say lines; they stay visible so the assistant
// always sees what it last told the user.
func dialogTail(tape []Line, count int) []int64 {
	var ids []int64

	for _, line := range slices.Backward(tape) {
		if len(ids) == count {
			break
		}

		if line.Kind == userKind || line.Kind == sayField {
			ids = append(ids, line.ID)
		}
	}

	return ids
}

func splitAround(span [2]int64, keep []int64) [][2]int64 {
	if span[0] > span[1] {
		return nil
	}

	for _, id := range keep {
		if span[0] <= id && id <= span[1] {
			return append(splitAround([2]int64{span[0], id - 1}, keep), splitAround([2]int64{id + 1, span[1]}, keep)...)
		}
	}

	return [][2]int64{span}
}

func (l *loop) rejectStep(ctx context.Context, s step, reason string, final bool) error {
	reason = logger.Redact(reason)

	outcome := "retry"
	if final {
		outcome = "failed"
	}

	err := l.deps.Store.WithTx(ctx, func(tx *sql.Tx) error {
		now := l.deps.Now()
		if _, err := AppendTapeTx(
			ctx,
			tx,
			l.sessionID,
			Line{At: now, Kind: "error", Text: "! error: " + reason},
		); err != nil {
			return err
		}

		if err := RecordTurnTx(ctx, tx, Turn{
			SessionID: l.sessionID, InputID: s.inputID, Step: s.number,
			Desk: s.render, Response: s.raw, Outcome: outcome, CreatedAt: now,
		}); err != nil {
			return err
		}

		if !final {
			return nil
		}

		if err := sessionstore.HandleAssistantInputTx(
			ctx,
			tx,
			l.sessionID,
			s.inputID,
			"assistant_turn_failed",
			now,
		); err != nil {
			return fmt.Errorf("fail assistant input: %w", err)
		}

		return l.deps.SayTx(ctx, tx, "⚠️ turn failed: "+reason)
	})
	if err != nil {
		return fmt.Errorf("reject assistant step: %w", err)
	}

	if final {
		l.deps.Notify()
	}

	return nil
}
