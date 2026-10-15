package assistant

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"github.com/pilat/coagent/internal/logger"
)

func (l *loop) applyActions(ctx context.Context, s step) (*Event, error) {
	var reads, labels []string

	for _, action := range s.answer.Do {
		text, err := l.applyAction(ctx, action)
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("assistant action interrupted: %w", ctx.Err())
			}

			reason := "! error: " + actionFailureName(action, s.board) + " failed: " + logger.Redact(err.Error())
			if err := l.actionFailure(ctx, action, reason); err != nil {
				return nil, err
			}

			continue
		}

		if action.Read != nil {
			label := actionLine(action, s.board)
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

	return &Event{LineID: id, At: l.deps.Now(), Kind: observeKind, Text: strings.Join(reads, "\n\n")}, nil
}

func (l *loop) applyAction(ctx context.Context, action Action) (string, error) {
	switch {
	case action.Open != nil:
		card, err := l.deps.Sessions.Open(ctx, action.Open.Project, action.Open.Text)
		if err != nil {
			return "", fmt.Errorf("open work session: %w", err)
		}

		if err := l.bindOpened(ctx, action.Open.Project, card.ID); err != nil {
			return "", err
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

// bindOpened moves stickers pinned to an opened project onto the session it started.
func (l *loop) bindOpened(ctx context.Context, project string, id int64) error {
	desk, err := l.deps.Memory.LoadDesk(ctx, l.sessionID)
	if err != nil {
		return fmt.Errorf("load desk to bind opened session: %w", err)
	}

	bound := false

	for i := range desk.Stickers {
		if desk.Stickers[i].On == project {
			desk.Stickers[i].On = strconv.FormatInt(id, 10)
			bound = true
		}
	}

	if !bound {
		return nil
	}

	desk.UpdatedAt = l.deps.Now()

	err = l.deps.Store.WithTx(ctx, func(tx *sql.Tx) error { return SaveDeskTx(ctx, tx, l.sessionID, desk) })
	if err != nil {
		return fmt.Errorf("bind stickers to opened session: %w", err)
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

func actionLine(a Action, board []Card) string {
	id := actionSession(a)
	var project string

	for _, card := range board {
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

func actionFailureName(a Action, board []Card) string {
	if a.Open != nil || a.Read != nil {
		return actionLine(a, board)
	}

	id := actionSession(a)
	var project string

	for _, card := range board {
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
