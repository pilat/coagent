package assistant

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionstore"
)

// apply runs the committed effects in call order; a bookkeeping error never stops the effects after it.
func (l *loop) apply(ctx context.Context, t *turn) error {
	opened := map[string]Card{}
	failed := map[string]bool{}
	handover, words := t.handoverQuote()

	var errs []error

	for i, e := range t.effects {
		var err error

		text := e.text
		if e == handover {
			text = withUserWords(text, words)
		}

		switch e.kind {
		case effectOpen:
			err = l.applyOpen(ctx, t, i, text, opened, failed)
		case effectSend:
			err = l.applySend(ctx, e, text, opened, failed)
		default:
			if cause := l.store.Stop(ctx, l.host, l.scope, e.target.ID); cause != nil {
				err = l.actionFailure(ctx, e.target.ID, fmt.Sprintf("! error: stop %s (%d) failed: %s",
					e.target.Project, e.target.ID, logger.Redact(cause.Error())))
			}
		}

		if err != nil {
			logger.Ctx(ctx).Named("assistant.apply").Error("apply effect", zap.String("kind", e.kind), zap.Error(err))
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

func (l *loop) applyOpen(
	ctx context.Context,
	t *turn,
	index int,
	text string,
	opened map[string]Card,
	failed map[string]bool,
) error {
	e := t.effects[index]

	card, err := l.store.Open(ctx, l.host, l.scope, e.project, text, e.about)
	if err != nil {
		failed[e.handle] = true
		reason := []string{fmt.Sprintf("! error: open %s failed: %s", e.project, logger.Redact(err.Error()))}

		for _, later := range t.effects[index+1:] {
			if later.kind == effectSend && later.handle == e.handle {
				reason = append(reason, fmt.Sprintf("skipped send to it: %q", Clip(later.text, 300)))
			}
		}

		resolveErr := l.resolveHandle(ctx, t, e.handle, "")

		return errors.Join(resolveErr, l.actionFailure(ctx, 0, strings.Join(reason, "; ")))
	}

	opened[e.handle] = card
	resolveErr := l.resolveHandle(ctx, t, e.handle, strconv.FormatInt(card.ID, 10))
	_, err = l.appendLine(ctx, moveKind, sendLine(card, e.text))

	return errors.Join(resolveErr, err)
}

func (l *loop) applySend(
	ctx context.Context,
	e *effect,
	text string,
	opened map[string]Card,
	failed map[string]bool,
) error {
	target := e.target
	if e.handle != "" {
		if failed[e.handle] {
			return nil
		}

		target = opened[e.handle]
	}

	if err := l.store.Send(ctx, l.host, l.scope, target.ID, text); err != nil {
		return l.actionFailure(ctx, target.ID, fmt.Sprintf("! error: send %s (%d) failed: %s",
			target.Project, target.ID, logger.Redact(err.Error())))
	}

	if e.handle == "" {
		return nil
	}

	_, err := l.appendLine(ctx, moveKind, sendLine(target, e.text))

	return err
}

func (t *turn) handoverQuote() (*effect, string) {
	if t.input.Source != sessionstore.InputSourceUser {
		return nil, ""
	}

	words := strings.ReplaceAll(strings.TrimSpace(t.input.RawContent), "\r\n", "\n")
	if words == "" {
		return nil, ""
	}

	var handover *effect

	for _, e := range t.effects {
		if e.kind != effectOpen && e.kind != effectSend {
			continue
		}

		if handover != nil {
			return nil, ""
		}

		handover = e
	}

	if handover == nil || (handover.kind == effectSend && handover.handle != "") {
		return nil, ""
	}

	for _, e := range t.effects {
		if e.kind == effectStop && (handover.kind == effectOpen || e.target.ID != handover.target.ID) {
			return nil, ""
		}
	}

	return handover, words
}

func withUserWords(text, words string) string {
	if words == "" || strings.HasPrefix(strings.TrimSpace(text), "/") ||
		strings.Contains(text, words) {
		return text
	}

	lines := strings.Split(words, "\n")
	for i, line := range lines {
		lines[i] = ">"
		if line != "" {
			lines[i] += " " + line
		}
	}

	return text + "\n\nThe user's own words:\n" + strings.Join(lines, "\n")
}

// resolveHandle rewrites this turn's handle in the committed focus refs; an empty id drops it.
func (l *loop) resolveHandle(ctx context.Context, t *turn, handle, id string) error {
	err := l.store.sessions.WithTx(ctx, func(tx *sql.Tx) error {
		for _, item := range t.focus {
			if item.deleted || item.ID == 0 || !slices.Contains(item.Sessions, handle) {
				continue
			}

			var sessions []string

			for _, ref := range item.Sessions {
				switch {
				case ref != handle:
					sessions = append(sessions, ref)
				case id != "" && !slices.Contains(item.Sessions, id):
					sessions = append(sessions, id)
				}
			}

			item.Sessions = sessions
			if err := setFocusSessionsTx(ctx, tx, l.scope.Root, item.ID, sessions); err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("resolve assistant handle %s: %w", handle, err)
	}

	return nil
}

func (l *loop) actionFailure(ctx context.Context, target int64, reason string) error {
	if _, err := l.appendLine(ctx, errorKind, reason); err != nil {
		return err
	}

	if _, err := l.store.sessions.Enqueue(ctx, eventInput(l.scope.Root, target, "action_failed", reason)); err != nil {
		return fmt.Errorf("enqueue assistant action failure: %w", err)
	}

	return nil
}
