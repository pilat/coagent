package assistant

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/pilat/coagent/internal/llmwire"
)

type modelArgs struct {
	Session sessionRef `json:"session"`
	Model   string     `json:"model"`
	Effort  *string    `json:"effort"`
}

func (t *assistantTool) switchChat(ctx context.Context, call llmwire.ToolCall) (string, error) {
	var args sendArgs
	if err := decodeArgs(call, &args); err != nil {
		return "", err
	}

	if args.Session == "0" || args.Session == "#0" {
		return "", errors.New("you cannot switch the user to Assistant; they return with /assistant")
	}

	id, ok := args.Session.id()
	if !ok {
		return "", fmt.Errorf("session %q must be a session id", args.Session)
	}

	if _, err := t.store.Target(ctx, t.scope, id); err != nil {
		return "", err
	}

	if err := t.host.FocusChat(ctx, t.scope.Owner, id); err != nil {
		return "", err //nolint:wrapcheck // Host refusals are the model-facing response.
	}

	return fmt.Sprintf("switched the user to session %d; they return with /assistant", id), nil
}

func (t *assistantTool) models(ctx context.Context) (string, error) {
	models, err := t.host.Models(ctx)
	if err != nil {
		return "", fmt.Errorf("list models: %w", err)
	}

	lines := make([]string, 0, len(models))
	for i, model := range models {
		line := model.ID + " · " + cmp.Or(model.DisplayName, model.ID)
		if i == 0 {
			line += " · default"
		}

		if len(model.EffortLevels) == 0 {
			line += " · no effort choice"
		} else {
			line += " · efforts: " + strings.Join(
				model.EffortLevels,
				", ",
			) + " · default effort: " + model.DefaultEffort
		}

		if model.Reasoning {
			line += " · reasoning"
		}

		if model.Priced {
			line += fmt.Sprintf(" · $%g/$%g", model.InputPrice, model.OutputPrice)
		} else {
			line += " · unpriced"
		}

		lines = append(lines, line)
	}

	return strings.Join(lines, "\n"), nil
}

func (t *assistantTool) setModel(ctx context.Context, call llmwire.ToolCall) (string, error) {
	var args modelArgs
	if err := decodeArgs(call, &args); err != nil {
		return "", err
	}

	id, ok := args.Session.id()
	if !ok {
		return "", fmt.Errorf("session %q must be a session id", args.Session)
	}

	if _, err := t.store.Target(ctx, t.scope, id); err != nil {
		return "", err
	}

	model, effort, err := t.modelChoice(ctx, args)
	if err != nil {
		return "", err
	}

	if err := t.host.SetModel(ctx, id, model.ID, effort); err != nil {
		return "", fmt.Errorf("set session model: %w", err)
	}

	line := fmt.Sprintf("session %d now runs %s", id, cmp.Or(model.DisplayName, model.ID))
	if effort != "" {
		line += " · " + effort
	}

	return line, nil
}

func (t *assistantTool) modelChoice(ctx context.Context, args modelArgs) (Model, string, error) {
	models, err := t.host.Models(ctx)
	if err != nil {
		return Model{}, "", fmt.Errorf("list models: %w", err)
	}

	for _, model := range models {
		if model.ID != args.Model {
			continue
		}

		if args.Effort == nil {
			return model, model.DefaultEffort, nil
		}

		if !slices.Contains(model.EffortLevels, *args.Effort) {
			return Model{}, "", fmt.Errorf("allowed effort levels for %s: %s", model.ID,
				cmp.Or(strings.Join(model.EffortLevels, ", "), "none"))
		}

		return model, *args.Effort, nil
	}

	return Model{}, "", fmt.Errorf("unknown model: %s", args.Model)
}
