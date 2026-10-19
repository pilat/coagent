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
)

// apply runs the committed effects in call order; a bookkeeping error never stops the effects after it.
func (l *loop) apply(ctx context.Context, t *turn) error {
	opened := map[string]Card{}
	failed := map[string]bool{}

	var errs []error

	for i, e := range t.effects {
		var err error

		switch e.kind {
		case effectOpen:
			err = l.applyOpen(ctx, t, i, opened, failed)
		case effectSend:
			err = l.applySend(ctx, e, opened, failed)
		default:
			if cause := l.deps.Sessions.Stop(ctx, e.target.ID); cause != nil {
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
	opened map[string]Card,
	failed map[string]bool,
) error {
	e := t.effects[index]

	card, err := l.deps.Sessions.Open(ctx, e.project, e.text, e.about)
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

func (l *loop) applySend(ctx context.Context, e *effect, opened map[string]Card, failed map[string]bool) error {
	target := e.target
	if e.handle != "" {
		if failed[e.handle] {
			return nil
		}

		target = opened[e.handle]
	}

	if err := l.deps.Sessions.Send(ctx, target.ID, e.text); err != nil {
		return l.actionFailure(ctx, target.ID, fmt.Sprintf("! error: send %s (%d) failed: %s",
			target.Project, target.ID, logger.Redact(err.Error())))
	}

	if e.handle == "" {
		return nil
	}

	_, err := l.appendLine(ctx, moveKind, sendLine(target, e.text))

	return err
}

// resolveHandle rewrites this turn's handle in the committed focus refs; an empty id drops it.
func (l *loop) resolveHandle(ctx context.Context, t *turn, handle, id string) error {
	err := l.deps.Store.WithTx(ctx, func(tx *sql.Tx) error {
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
			if err := setFocusSessionsTx(ctx, tx, l.sessionID, item.ID, sessions); err != nil {
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

	if err := l.deps.Enqueue(ctx, "action_failed", target, reason); err != nil {
		return fmt.Errorf("enqueue assistant action failure: %w", err)
	}

	return nil
}
