package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/managers/cli"
)

func (c *chat) chooseModel(ctx context.Context) error {
	var result cli.ModelsResult
	if err := c.callIdempotent(
		ctx,
		cli.OpChatModels,
		func() any { return cli.SessionParams{SessionID: c.currentSession()} },
		&result,
	); err != nil {
		return err
	}

	if len(result.Models) == 0 {
		c.println("No models configured.")
		c.prompt()

		return nil
	}

	current := result.CurrentID
	if pending := c.pendingModel(); pending != "" && c.currentSession() == 0 {
		current = pending
	}

	model, selected, err := c.pickModel(ctx, result.Models, current)
	if err != nil || !selected {
		c.prompt()

		return err
	}

	currentEffort := ""
	if model.ID == result.CurrentID {
		currentEffort = result.CurrentEffort
	}

	return c.applyModelChoice(ctx, model, currentEffort)
}

func (c *chat) applyModelChoice(
	ctx context.Context,
	model controllerapi.ConfigModelInfo,
	currentEffort string,
) error {
	if c.currentSession() == 0 {
		c.setPendingModel(model.ID)
		c.println("New conversation model: " + modelName(model))
		c.prompt()

		return nil
	}

	effort, err := c.pickEffort(ctx, model, currentEffort)
	if err != nil {
		return err
	}

	params := func() any {
		return cli.SetModelParams{
			SessionID: c.currentSession(), Model: model.ID, ReasoningLevel: effort,
		}
	}
	if err := c.callIdempotent(ctx, cli.OpChatSetModel, params, nil); err != nil {
		return err
	}

	c.setPendingModel("")

	message := "Model: " + modelName(model)
	if effort != "" {
		message += " · effort: " + effort
	}

	c.println(message)
	c.prompt()

	return nil
}

func (c *chat) pickModel(
	ctx context.Context,
	models []controllerapi.ConfigModelInfo,
	current string,
) (controllerapi.ConfigModelInfo, bool, error) {
	c.println("Models:")

	for i, model := range models {
		marker := ""
		if model.ID == current {
			marker = " (current)"
		}

		c.println(fmt.Sprintf("  %d) %s%s", i+1, modelName(model), marker))
	}

	for {
		choice, err := c.readChoice(ctx, "Choose model [Enter to cancel]: ")
		if err != nil {
			return controllerapi.ConfigModelInfo{}, false, err
		}

		if choice == "" {
			return controllerapi.ConfigModelInfo{}, false, nil
		}

		if model, ok := resolveModelChoice(models, choice); ok {
			return model, true, nil
		}

		c.println(fmt.Sprintf("Choose a number from 1 to %d, or enter a model id.", len(models)))
	}
}

func (c *chat) pickEffort(
	ctx context.Context,
	model controllerapi.ConfigModelInfo,
	current string,
) (string, error) {
	if len(model.EffortLevels) == 0 {
		return "", nil
	}

	c.println("Reasoning effort:")

	for i, effort := range model.EffortLevels {
		var markers []string
		if effort == model.DefaultEffort {
			markers = append(markers, "default")
		}

		if effort == current {
			markers = append(markers, "current")
		}

		marker := ""
		if len(markers) > 0 {
			marker = " (" + strings.Join(markers, ", ") + ")"
		}

		c.println(fmt.Sprintf("  %d) %s%s", i+1, effort, marker))
	}

	for {
		choice, err := c.readChoice(ctx, "Choose effort [Enter for default]: ")
		if err != nil {
			return "", err
		}

		if choice == "" {
			return model.DefaultEffort, nil
		}

		if effort, ok := resolveStringChoice(model.EffortLevels, choice); ok {
			return effort, nil
		}

		c.println(fmt.Sprintf("Choose a number from 1 to %d, or enter an effort name.", len(model.EffortLevels)))
	}
}

// readChoice keeps the input loop as the terminal's only reader. A secret that
// arrives while the picker is open still owns the typed line, just as in chat.
func (c *chat) readChoice(ctx context.Context, prompt string) (string, error) {
	c.write(prompt)

	for {
		if err := c.takeFatal(); err != nil {
			return "", err
		}

		if req, ok := c.takeSecret(); ok {
			if err := c.askForSecret(ctx, req, ""); err != nil {
				return "", err
			}

			c.write(prompt)

			continue
		}

		line, err := c.term.ReadLine()
		switch {
		case errors.Is(err, errNoInput):
			continue
		case errors.Is(err, io.EOF):
			return "", io.EOF
		case err != nil:
			return "", fmt.Errorf("read: %w", err)
		}

		line = strings.TrimSpace(line)
		if req, ok := c.takeSecret(); ok {
			if err := c.askForSecret(ctx, req, line); err != nil {
				return "", err
			}

			c.write(prompt)

			continue
		}

		return line, nil
	}
}

func resolveModelChoice(
	models []controllerapi.ConfigModelInfo,
	choice string,
) (controllerapi.ConfigModelInfo, bool) {
	if index, ok := resolveIndex(len(models), choice); ok {
		return models[index], true
	}

	for _, model := range models {
		if model.ID == choice {
			return model, true
		}
	}

	return controllerapi.ConfigModelInfo{}, false
}

func resolveStringChoice(values []string, choice string) (string, bool) {
	if index, ok := resolveIndex(len(values), choice); ok {
		return values[index], true
	}

	for _, value := range values {
		if value == choice {
			return value, true
		}
	}

	return "", false
}

func resolveIndex(length int, choice string) (int, bool) {
	index, err := strconv.Atoi(choice)
	if err != nil || index < 1 || index > length {
		return 0, false
	}

	return index - 1, true
}

func modelName(model controllerapi.ConfigModelInfo) string {
	if model.DisplayName != "" {
		return model.DisplayName
	}

	if model.Name != "" {
		return model.Name
	}

	return model.ID
}

func (c *chat) sendParams(line string) cli.SendParams {
	c.mu.Lock()
	defer c.mu.Unlock()

	params := cli.SendParams{SessionID: c.session, Text: line}
	if c.model != "" {
		params.SessionID = 0
		params.Model = c.model
	}

	return params
}

func (c *chat) pendingModel() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.model
}

func (c *chat) setPendingModel(model string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.model = model
}
