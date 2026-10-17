package assistant

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
)

func (l *loop) applyActions(ctx context.Context, s step) (*Event, error) {
	var reads, labels []string
	var readIDs []int64

	for _, action := range s.answer.Do {
		text, err := l.applyAction(ctx, s, action)
		if err != nil {
			if err := l.handleActionFailure(ctx, s, action, err); err != nil {
				return nil, err
			}

			continue
		}

		if action.Read != nil {
			readIDs = append(readIDs, action.Read.Session)
			label := actionLine(action, s.cards)
			labels = append(labels, label)
			reads = append(reads, label+"\n"+text)
		}
	}

	if len(reads) == 0 {
		return nil, nil //nolint:nilnil // No read means this turn has no next step.
	}

	id, err := l.appendLine(ctx, observeKind, strings.Join(labels, "; "))
	if err != nil {
		return nil, err
	}

	return &Event{
		LineID:   id,
		At:       l.deps.Now(),
		Kind:     observeKind,
		Text:     strings.Join(reads, "\n\n"),
		Sessions: readIDs,
	}, nil
}

func (l *loop) handleActionFailure(ctx context.Context, s step, action Action, cause error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("assistant action interrupted: %w", ctx.Err())
	}

	reason := "! error: " + actionFailureName(action, s.cards) + " failed: " + logger.Redact(cause.Error())
	if action.Open != nil {
		if err := l.discardOpened(ctx, s, action.Open.Project); err != nil {
			return err
		}
	}

	return l.actionFailure(ctx, action, reason)
}

func (l *loop) applyAction(ctx context.Context, s step, action Action) (string, error) {
	switch {
	case action.Open != nil:
		card, err := l.deps.Sessions.Open(ctx, action.Open.Project, action.Open.Text, action.Open.About)
		if err != nil {
			return "", fmt.Errorf("open work session: %w", err)
		}

		if err := l.bindOpened(ctx, s, action.Open.Project, card); err != nil {
			logger.Ctx(ctx).
				Named("assistant.open").
				Error("bind notes after open", zap.Int64("session_id", card.ID), zap.Error(err))
		}

		_, err = l.appendLine(
			ctx,
			"move",
			fmt.Sprintf("→ %s (%d): %q", card.Project, card.ID, Clip(action.Open.Text, 300)),
		)

		return "", err
	case action.Send != nil:
		if err := l.deps.Sessions.Send(ctx, action.Send.Session, action.Send.Text); err != nil {
			return "", fmt.Errorf("send to work session: %w", err)
		}
	case action.Stop != nil:
		if err := l.deps.Sessions.Stop(ctx, action.Stop.Session); err != nil {
			return "", fmt.Errorf("stop work session: %w", err)
		}
	case action.Read != nil:
		text, err := l.deps.Sessions.Read(ctx, action.Read.Session, action.Read.Lines)
		if err != nil {
			return "", fmt.Errorf("read work session: %w", err)
		}

		return text, nil
	}

	return "", nil
}

func (l *loop) bindOpened(ctx context.Context, s step, raw string, card Card) error {
	err := l.deps.Store.WithTx(ctx, func(tx *sql.Tx) error {
		if s.answer.Notes["wait."+raw] != nil {
			if err := rekeyNoteTx(
				ctx,
				tx,
				l.sessionID,
				"wait."+raw,
				"wait."+strconv.FormatInt(card.ID, 10),
			); err != nil {
				return err
			}
		}

		if !s.shown.ProjectKeys[raw] && s.answer.Notes["project."+raw] != nil {
			if err := rekeyNoteTx(ctx, tx, l.sessionID, "project."+raw, "project."+card.ProjectKey); err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("bind opened notes: %w", err)
	}

	return nil
}

func (l *loop) discardOpened(ctx context.Context, s step, raw string) error {
	edits := map[string]*string{}
	if s.answer.Notes["wait."+raw] != nil {
		edits["wait."+raw] = nil
	}

	if !s.shown.ProjectKeys[raw] && s.answer.Notes["project."+raw] != nil {
		edits["project."+raw] = nil
	}

	if err := l.deps.Store.WithTx(ctx, func(tx *sql.Tx) error {
		return SaveNotesTx(ctx, tx, l.sessionID, edits, l.deps.Now())
	}); err != nil {
		return fmt.Errorf("discard failed open notes: %w", err)
	}

	return nil
}

func (l *loop) actionFailure(ctx context.Context, action Action, reason string) error {
	if _, err := l.appendLine(ctx, "error", reason); err != nil {
		return err
	}

	if err := l.deps.Enqueue(ctx, "action_failed", actionSession(action), reason); err != nil {
		return fmt.Errorf("enqueue assistant action failure: %w", err)
	}

	return nil
}

func actionLine(a Action, cards []Card) string {
	id := actionSession(a)
	var project string

	for _, card := range cards {
		if card.ID == id {
			project = card.Project
		}
	}

	switch {
	case a.Open != nil:
		return "open " + a.Open.Project
	case a.Send != nil:
		return fmt.Sprintf("→ %s (%d): %q", project, id, Clip(a.Send.Text, 300))
	case a.Read != nil:
		return fmt.Sprintf("read %s (%d)", project, id)
	default:
		return fmt.Sprintf("⏹ stopped %s (%d)", project, id)
	}
}

func actionSession(a Action) int64 {
	switch {
	case a.Send != nil:
		return a.Send.Session
	case a.Read != nil:
		return a.Read.Session
	case a.Stop != nil:
		return a.Stop.Session
	default:
		return 0
	}
}

func actionFailureName(a Action, cards []Card) string {
	if a.Open != nil || a.Read != nil {
		return actionLine(a, cards)
	}

	id := actionSession(a)
	var project string

	for _, card := range cards {
		if card.ID == id {
			project = card.Project
		}
	}

	name := "stop"
	if a.Send != nil {
		name = "send"
	}

	return fmt.Sprintf("%s %s (%d)", name, project, id)
}
