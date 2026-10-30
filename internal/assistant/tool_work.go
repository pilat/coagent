package assistant

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

func (t *assistantTool) work(ctx context.Context, call llmwire.ToolCall) (string, error) {
	if call.Name == toolOpen {
		return t.open(ctx, call)
	}

	var args sendArgs
	if err := decodeArgs(call, &args); err != nil {
		return "", err
	}

	id, ok := args.Session.id()
	if !ok {
		return "", fmt.Errorf("session %q must be a session id", args.Session)
	}

	if call.Name == toolStop {
		if err := t.store.Stop(ctx, t.host, t.scope, id); err != nil {
			return "", err
		}

		return fmt.Sprintf("stopped session %d", id), nil
	}

	if strings.TrimSpace(args.Text) == "" {
		return "", errors.New("send needs a text")
	}

	if err := t.store.Send(ctx, t.host, t.scope, id, strings.TrimSpace(args.Text)); err != nil {
		return "", err
	}

	return fmt.Sprintf("sent to session %d", id), nil
}

func (t *assistantTool) open(ctx context.Context, call llmwire.ToolCall) (string, error) {
	var args openArgs
	if err := decodeArgs(call, &args); err != nil {
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

	model, effort, err := t.openModelChoice(ctx, args)
	if err != nil {
		return "", err
	}

	card, err := t.store.Open(ctx, t.host, t.scope, args.Project, args.Text, title, model, effort, args.Worktree)
	if err != nil {
		return "", err
	}

	models, err := t.host.Models(ctx)
	if err != nil {
		return "", fmt.Errorf("list models: %w", err)
	}

	return cardLines(card, time.Now().UTC(), models), nil
}

func (t *assistantTool) openModelChoice(ctx context.Context, args openArgs) (string, string, error) {
	if args.Model == "" && args.Effort == nil {
		return "", "", nil
	}

	if args.Model == "" {
		models, err := t.host.Models(ctx)
		if err != nil {
			return "", "", fmt.Errorf("list models: %w", err)
		}

		if len(models) == 0 {
			return "", "", errors.New("no models configured")
		}

		args.Model = models[0].ID
	}

	model, effort, err := t.modelChoice(ctx, modelArgs{Model: args.Model, Effort: args.Effort})
	if err != nil {
		return "", "", err
	}

	return model.ID, effort, nil
}
