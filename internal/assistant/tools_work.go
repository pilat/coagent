package assistant

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/pilat/coagent/internal/sessionstore"
)

func (t *assistantTool) open(ctx context.Context, raw json.RawMessage) (string, error) {
	var args openArgs
	if err := decodeArgs(t.ID(), raw, &args); err != nil {
		return "", err
	}

	args.Project = strings.TrimSpace(args.Project)

	args.Text = strings.TrimSpace(args.Text)
	if args.Project == "" || args.Text == "" {
		return "", errors.New("open needs a project and a text")
	}

	title, ok := sessionstore.NormalizeTitle(args.About)
	if !ok {
		return "", errors.New("about must be one line of 1-80 characters")
	}

	models, err := t.host.Models(ctx)
	if err != nil {
		return "", fmt.Errorf("list models: %w", err)
	}

	var model Model

	var effort string
	if args.Model != "" || args.Effort != nil {
		model, effort, err = chooseModel(models, args.Model, args.Effort)
		if err != nil {
			return "", err
		}
	}

	text := args.Text

	choice, err := t.store.openTarget(ctx, args.Project, args.Worktree != "")
	if err != nil {
		return "", err
	}

	id, name, err := t.host.CreateWork(ctx, t.scope.Owner, WorkSpec{
		ProjectID: choice.id, Name: choice.name, WorkDir: choice.path, Title: title,
		Model: model.ID, Effort: effort, Worktree: args.Worktree,
	}, text, func(tx *sql.Tx, id int64, attributes map[string]any) (*sessionstore.InboxInput, error) {
		return t.store.instructTx(ctx, tx, t.scope, id, text, attributes)
	})
	if err != nil {
		return "", fmt.Errorf("open assistant work: %w", err)
	}

	record, err := t.store.sessions.GetSession(ctx, id)
	if err != nil {
		return "", fmt.Errorf("load opened session model: %w", err)
	}

	now := time.Now().UTC()
	card := Card{
		ID: id, Project: name, Status: workingStatus, CreatedAt: now, Activity: now,
		Title: title, Model: record.Model, Effort: record.ReasoningLevel,
	}

	return cardLines(card, now, models), nil
}

func (t *assistantTool) send(ctx context.Context, raw json.RawMessage) (string, error) {
	var args sendArgs
	if err := decodeArgs(t.ID(), raw, &args); err != nil {
		return "", err
	}

	id, err := args.Session.parse()
	if err != nil {
		return "", err
	}

	if strings.TrimSpace(args.Text) == "" {
		return "", errors.New("send needs a text")
	}

	text := strings.TrimSpace(args.Text)

	if err := t.deliver(ctx, id, text); err != nil {
		return "", err
	}

	return fmt.Sprintf("sent to session %d", id), nil
}

func (t *assistantTool) forward(ctx context.Context, raw json.RawMessage) (string, error) {
	var args forwardArgs
	if err := decodeArgs(t.ID(), raw, &args); err != nil {
		return "", err
	}

	card, err := t.target(ctx, args.Session)
	if err != nil {
		return "", err
	}

	input, err := t.store.sessions.ActiveHumanInput(ctx, t.scope.Root)
	if err != nil {
		return "", fmt.Errorf("load current human input: %w", err)
	}

	if input == nil {
		return "", sessionstore.ErrForwardInput
	}

	if strings.HasPrefix(strings.TrimSpace(input.RawContent), "/") {
		return "", sessionstore.ErrForwardedCommand
	}

	err = t.host.Deliver(
		ctx,
		card.ID,
		input.RawContent,
		func(tx *sql.Tx, attributes map[string]any) (*sessionstore.InboxInput, error) {
			return t.store.forwardTx(ctx, tx, t.scope, card.ID, attributes)
		},
	)
	if err != nil {
		return "", fmt.Errorf("deliver forwarded input: %w", err)
	}

	return fmt.Sprintf("forwarded the user's message to session %d", card.ID), nil
}

func (t *assistantTool) stop(ctx context.Context, raw json.RawMessage) (string, error) {
	var args sendArgs
	if err := decodeArgs(t.ID(), raw, &args); err != nil {
		return "", err
	}

	card, err := t.target(ctx, args.Session)
	if err != nil {
		return "", err
	}

	if err := t.host.Stop(ctx, card.ID); err != nil {
		return "", fmt.Errorf("stop assistant work: %w", err)
	}

	return fmt.Sprintf("stopped session %d", card.ID), nil
}

func (t *assistantTool) kill(ctx context.Context, raw json.RawMessage) (string, error) {
	var args sendArgs
	if err := decodeArgs(t.ID(), raw, &args); err != nil {
		return "", err
	}

	card, err := t.target(ctx, args.Session)
	if err != nil {
		return "", err
	}

	if err := t.host.Kill(ctx, card.ID); err != nil {
		return "", fmt.Errorf("kill assistant work: %w", err)
	}

	return fmt.Sprintf("deleted session %d", card.ID), nil
}

func (t *assistantTool) target(ctx context.Context, ref sessionRef) (Card, error) {
	id, err := ref.parse()
	if err != nil {
		return Card{}, err
	}

	return t.store.Target(ctx, t.scope, id)
}

func (t *assistantTool) deliver(ctx context.Context, id int64, text string) error {
	err := t.host.Deliver(ctx, id, text, func(tx *sql.Tx, attributes map[string]any) (*sessionstore.InboxInput, error) {
		return t.store.instructTx(ctx, tx, t.scope, id, text, attributes)
	})
	if err != nil {
		return fmt.Errorf("deliver assistant instruction: %w", err)
	}

	return nil
}

func (t *assistantTool) switchChat(ctx context.Context, raw json.RawMessage) (string, error) {
	var args sendArgs
	if err := decodeArgs(t.ID(), raw, &args); err != nil {
		return "", err
	}

	if args.Session == "0" || args.Session == "#0" {
		return "", errors.New("you cannot switch the user to Assistant; they return with /assistant")
	}

	card, err := t.target(ctx, args.Session)
	if err != nil {
		return "", err
	}

	if err := t.host.FocusChat(ctx, t.scope.Owner, card.ID); err != nil {
		return "", err //nolint:wrapcheck // Host refusals are the model-facing response.
	}

	return fmt.Sprintf("switched the user to session %d; they return with /assistant", card.ID), nil
}

func (t *assistantTool) setModel(ctx context.Context, raw json.RawMessage) (string, error) {
	var args modelArgs
	if err := decodeArgs(t.ID(), raw, &args); err != nil {
		return "", err
	}

	card, err := t.target(ctx, args.Session)
	if err != nil {
		return "", err
	}

	models, err := t.host.Models(ctx)
	if err != nil {
		return "", fmt.Errorf("list models: %w", err)
	}

	if args.Model == "" {
		return "", fmt.Errorf("unknown model: %s", args.Model)
	}

	model, effort, err := chooseModel(models, args.Model, args.Effort)
	if err != nil {
		return "", err
	}

	if err := t.host.SetModel(ctx, card.ID, model.ID, effort); err != nil {
		return "", fmt.Errorf("set session model: %w", err)
	}

	if effort != "" {
		effort = " · " + effort
	}

	return fmt.Sprintf("session %d now runs %s%s", card.ID, cmp.Or(model.DisplayName, model.ID), effort), nil
}

func chooseModel(models []Model, id string, effort *string) (Model, string, error) {
	if id == "" && len(models) == 0 {
		return Model{}, "", errors.New("no models configured")
	}

	for _, model := range models {
		if id != "" && model.ID != id {
			continue
		}

		if effort == nil {
			return model, model.DefaultEffort, nil
		}

		if !slices.Contains(model.EffortLevels, *effort) {
			return Model{}, "", fmt.Errorf("allowed effort levels for %s: %s", model.ID,
				cmp.Or(strings.Join(model.EffortLevels, ", "), "none"))
		}

		return model, *effort, nil
	}

	return Model{}, "", fmt.Errorf("unknown model: %s", id)
}
