package assistant

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

// Run drains FIFO inputs without entering the ordinary session runtime.
func Run(ctx context.Context, deps Deps, sessionID int64) error {
	l := loop{deps: deps, sessionID: sessionID}
	if err := deps.Store.UpdateSessionStatus(ctx, sessionID, sessionstore.SessionStatusActive); err != nil {
		return fmt.Errorf("activate assistant: %w", err)
	}

	err := l.drain(ctx)
	finishErr := deps.Store.UpdateSessionStatus(
		context.WithoutCancel(ctx),
		sessionID,
		sessionstore.SessionStatusCompleted,
	)

	return errors.Join(err, finishErr)
}

func (l *loop) drain(ctx context.Context) error {
	for ctx.Err() == nil {
		inputs, err := l.deps.Store.ListPending(ctx, l.sessionID)
		if err != nil {
			return fmt.Errorf("list assistant inputs: %w", err)
		}

		if len(inputs) == 0 {
			return nil
		}

		for _, input := range inputs {
			if err := l.turn(ctx, input); err != nil {
				return err
			}
		}
	}

	return fmt.Errorf("assistant interrupted: %w", ctx.Err())
}

func (l *loop) turn(ctx context.Context, input *sessionstore.InboxInput) error {
	if input.Source == sessionstore.InputSourceUser && strings.HasPrefix(strings.TrimSpace(input.RawContent), "/") {
		err := l.deps.Store.WithTx(ctx, func(tx *sql.Tx) error {
			return sessionstore.HandleAssistantInputTx(ctx, tx, l.sessionID, input.ID,
				"assistant_ignored_command", l.deps.Now())
		})
		if errors.Is(err, sessionstore.ErrInputResolved) {
			return nil
		}

		if err != nil {
			return fmt.Errorf("ignore assistant command: %w", err)
		}

		return nil
	}

	event, err := l.event(ctx, input)
	if err != nil {
		return err
	}

	t, err := l.begin(ctx, input, event)
	if err != nil {
		return err
	}

	if err := l.mainPhase(ctx, t); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("assistant model interrupted: %w", ctx.Err())
		}

		return l.fail(ctx, t, err.Error())
	}

	reply, err := l.consolidate(ctx, t)
	if err != nil {
		return err
	}

	outcome, err := l.guard(ctx, t)
	if err != nil {
		return err
	}

	if err := l.commit(ctx, t, reply, outcome); err != nil {
		return err
	}

	return l.apply(context.WithoutCancel(ctx), t)
}

func (l *loop) event(ctx context.Context, input *sessionstore.InboxInput) (Event, error) {
	event := Event{Kind: observeKind, Text: input.RawContent}
	tapeText := Clip(event.Text, 300)

	if input.Source == sessionstore.InputSourceUser {
		event.Kind = userKind
		event.Text = "user: «" + input.RawContent + "»"
		tapeText = event.Text
	}

	if kind, _ := input.Attributes["assistant_event"].(string); kind == "action_failed" {
		event.Kind = errorKind
	}

	note, err := l.alreadyTold(ctx, input)
	if err != nil {
		return Event{}, err
	}

	if note != "" {
		event.Text += "\n\n" + note
	}

	event.LineID, err = l.appendLine(ctx, event.Kind, tapeText)
	if err != nil {
		return Event{}, err
	}

	return event, nil
}

func (l *loop) begin(ctx context.Context, input *sessionstore.InboxInput, event Event) (*turn, error) {
	now := l.deps.Now()

	view, err := loadView(ctx, l.deps.Memory, l.deps.Sessions, l.sessionID, event, now)
	if err != nil {
		return nil, fmt.Errorf("load assistant desk: %w", err)
	}

	memory, err := l.deps.Memory.Memory(ctx, l.sessionID)
	if err != nil {
		return nil, fmt.Errorf("load assistant turn memory: %w", err)
	}

	t := newTurn(input, memory, view.Desk.Focus, now)
	t.messages = []llmwire.Message{{Role: llmwire.RoleUser, Content: Render(view, now)}}

	return t, nil
}

func (l *loop) appendLine(ctx context.Context, kind, text string) (int64, error) {
	var id int64

	err := l.deps.Store.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		id, err = AppendTapeTx(ctx, tx, l.sessionID, Line{At: l.deps.Now(), Kind: kind, Text: text})

		return err
	})
	if err != nil {
		return 0, fmt.Errorf("append assistant event: %w", err)
	}

	return id, nil
}
