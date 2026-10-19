package assistant

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionstore"
)

// commit applies the whole turn at once; the tape order matters to alreadyTold: reads, then the reply, then moves.
func (l *loop) commit(ctx context.Context, t *turn, reply, outcome string) error {
	err := l.deps.Store.WithTx(ctx, func(tx *sql.Tx) error {
		now := l.deps.Now()
		if err := l.saveMemory(ctx, tx, t, now); err != nil {
			return err
		}

		if err := l.saveFocus(ctx, tx, t, now); err != nil {
			return err
		}

		if err := l.commitTape(ctx, tx, t, reply, now); err != nil {
			return err
		}

		if err := l.recordCalls(ctx, tx, t, outcome); err != nil {
			return err
		}

		if err := sessionstore.HandleAssistantInputTx(
			ctx,
			tx,
			l.sessionID,
			t.input.ID,
			"assistant_turn",
			now,
		); err != nil {
			return fmt.Errorf("commit assistant input: %w", err)
		}

		if reply != "" {
			return l.deps.SayTx(ctx, tx, reply)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("commit assistant turn: %w", err)
	}

	if reply != "" {
		l.deps.Notify()
	}

	return nil
}

// fail discards the buffer: nothing was applied, which is the point of buffering.
func (l *loop) fail(ctx context.Context, t *turn, reason string) error {
	reason = logger.Redact(reason)

	err := l.deps.Store.WithTx(ctx, func(tx *sql.Tx) error {
		now := l.deps.Now()
		if _, err := AppendTapeTx(ctx, tx, l.sessionID,
			Line{At: now, Kind: errorKind, Text: "! error: " + reason}); err != nil {
			return err
		}

		if err := l.recordCalls(ctx, tx, t, "failed"); err != nil {
			return err
		}

		if err := sessionstore.HandleAssistantInputTx(
			ctx, tx, l.sessionID, t.input.ID, "assistant_turn_failed", now,
		); err != nil {
			return fmt.Errorf("fail assistant input: %w", err)
		}

		return l.deps.SayTx(ctx, tx, "⚠️ turn failed: "+reason)
	})
	if err != nil {
		return fmt.Errorf("fail assistant turn: %w", err)
	}

	l.deps.Notify()

	return nil
}

func (l *loop) saveMemory(ctx context.Context, tx *sql.Tx, t *turn, now time.Time) error {
	for _, item := range t.memory {
		var err error

		switch {
		case item.deleted && item.ID > 0:
			err = deleteMemoryTx(ctx, tx, l.sessionID, item.ID)
		case item.deleted || !item.changed:
		case item.ID == 0:
			item.ID, err = insertMemoryTx(ctx, tx, l.sessionID, item.Memory, now)
		default:
			err = updateMemoryTx(ctx, tx, l.sessionID, item.Memory, now)
		}

		if err != nil {
			return err
		}
	}

	return nil
}

func (l *loop) saveFocus(ctx context.Context, tx *sql.Tx, t *turn, now time.Time) error {
	for _, item := range t.focus {
		var err error

		switch {
		case item.deleted && item.ID > 0:
			err = deleteFocusTx(ctx, tx, l.sessionID, item.ID)
		case item.deleted || !item.changed:
		case item.ID == 0:
			item.ID, err = insertFocusTx(ctx, tx, l.sessionID, item.Focus, now)
		default:
			err = updateFocusTx(ctx, tx, l.sessionID, item.Focus, now)
		}

		if err != nil {
			return err
		}
	}

	return nil
}

func (l *loop) commitTape(ctx context.Context, tx *sql.Tx, t *turn, reply string, now time.Time) error {
	var lines []Line

	for _, card := range t.reads {
		lines = append(lines, Line{Kind: observeKind, Text: fmt.Sprintf("read %s (%d)", card.Project, card.ID)})
	}

	if reply != "" {
		lines = append(lines, Line{Kind: sayKind, Text: "say: «" + reply + "»"})
	}

	for _, e := range t.effects {
		switch {
		case e.kind == effectSend && e.handle == "":
			lines = append(lines, Line{Kind: moveKind, Text: sendLine(e.target, e.text)})
		case e.kind == effectStop:
			lines = append(lines, Line{
				Kind: moveKind,
				Text: fmt.Sprintf("⏹ stopped %s (%d)", e.target.Project, e.target.ID),
			})
		}
	}

	for _, line := range lines {
		line.At = now
		if _, err := AppendTapeTx(ctx, tx, l.sessionID, line); err != nil {
			return err
		}
	}

	return nil
}

func (l *loop) recordCalls(ctx context.Context, tx *sql.Tx, t *turn, outcome string) error {
	for _, call := range t.calls {
		call.Outcome = outcome
		if err := RecordCallTx(ctx, tx, call); err != nil {
			return err
		}
	}

	return nil
}

func sendLine(card Card, text string) string {
	return fmt.Sprintf("→ %s (%d): %q", card.Project, card.ID, Clip(text, 300))
}
