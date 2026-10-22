package assistant

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

// Host is the daemon lifecycle behind assistant roots; Deliver's enqueue authorizes, other ids are authorized first.
type Host interface {
	NewClient(sessionID int64, model, level string) (llm.Client, error)
	CreateWork(ctx context.Context, owner string, projectID int64, name, workDir string) (int64, error)
	// Deliver commits enqueue under the session's tree lock, then wakes the session.
	Deliver(ctx context.Context, sessionID int64, text string, enqueue func(*sql.Tx) error) error
	Stop(ctx context.Context, sessionID int64) error
	Notify(sessionID int64, text string)
}

type loop struct {
	store *Store
	host  Host
	scope Scope
}

// Run drains FIFO inputs without entering the ordinary session runtime.
func Run(ctx context.Context, store *Store, host Host, sessionID int64) error {
	record, err := store.sessions.GetSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("load assistant: %w", err)
	}

	owner, _ := record.Attributes[controllerapi.SessionAttributeManagerID].(string)
	l := loop{store: store, host: host, scope: Scope{Root: sessionID, Owner: owner}}

	if err := store.sessions.UpdateSessionStatus(ctx, l.scope.Root, sessionstore.SessionStatusActive); err != nil {
		return fmt.Errorf("activate assistant: %w", err)
	}

	err = l.drain(ctx)
	finishErr := store.sessions.UpdateSessionStatus(
		context.WithoutCancel(ctx),
		l.scope.Root,
		sessionstore.SessionStatusCompleted,
	)

	return errors.Join(err, finishErr)
}

func (l *loop) drain(ctx context.Context) error {
	for ctx.Err() == nil {
		inputs, err := l.store.sessions.ListPending(ctx, l.scope.Root)
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
		err := l.store.sessions.WithTx(ctx, func(tx *sql.Tx) error {
			return sessionstore.HandleAssistantInputTx(ctx, tx, l.scope.Root, input.ID,
				"assistant_ignored_command", time.Now().UTC())
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
	now := time.Now().UTC()

	desk, err := l.store.LoadDesk(ctx, l.scope.Root, event.LineID, now)
	if err != nil {
		return nil, fmt.Errorf("load assistant desk: read desk: %w", err)
	}

	recent, total, err := l.store.Recent(ctx, l.scope, RecentSessions)
	if err != nil {
		return nil, fmt.Errorf("load assistant desk: read recent sessions: %w", err)
	}

	memory, err := l.store.Memory(ctx, l.scope.Root)
	if err != nil {
		return nil, fmt.Errorf("load assistant turn memory: %w", err)
	}

	view := View{Desk: desk, Recent: recent, Total: total, Event: event}
	t := newTurn(input, memory, desk.Focus, now)
	t.messages = []llmwire.Message{{Role: llmwire.RoleUser, Content: Render(view, now)}}

	return t, nil
}

func (l *loop) appendLine(ctx context.Context, kind, text string) (int64, error) {
	var id int64

	err := l.store.sessions.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		id, err = AppendTapeTx(ctx, tx, l.scope.Root, Line{At: time.Now().UTC(), Kind: kind, Text: text})

		return err
	})
	if err != nil {
		return 0, fmt.Errorf("append assistant event: %w", err)
	}

	return id, nil
}
