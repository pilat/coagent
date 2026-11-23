package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/orchestrator"
	"github.com/pilat/coagent/internal/projectpath"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

func (h orchestratorHost) SpawnSession(
	ctx context.Context,
	callerID int64,
	request orchestrator.SpawnRequest,
) (int64, string, error) {
	caller, owner, err := h.caller(ctx, callerID)
	if err != nil {
		return 0, "", err
	}

	request.Model, request.Effort, err = h.modelChoice(request.Model, request.Effort)
	if err != nil {
		return 0, "", err
	}

	if free := h.s.runners.freeSlots(); free < spawnHeadroom {
		const full = "daemon is near its session capacity (%d of %d running); try again after some sessions finish"

		return 0, "", fmt.Errorf(full, maxTotal-free, maxTotal)
	}

	selected, err := h.project(ctx, request.Project)
	if err != nil {
		return 0, "", err
	}

	sender, err := h.provenance(ctx, caller)
	if err != nil {
		return 0, "", err
	}

	attrs := map[string]any{
		controllerapi.SessionAttributeManagerID: owner,
		controllerapi.SessionAttributeSpawnedBy: callerID,
	}
	if title := controllerapi.SessionTitleFromInput(request.Prompt); title != "" {
		attrs[controllerapi.SessionAttributeTitle] = title
	}

	return h.s.createManagerRoot(ctx, rootRequest{
		Root: sessionstore.ManagerRootCreate{
			ProjectID: selected.ID, Name: selected.Name, WorkDir: selected.WorkDir,
			Model: request.Model, ReasoningLevel: request.Effort, Attributes: attrs,
			Input: sessionstore.Input{
				Source: sessionstore.InputSourceAgent, Content: request.Prompt, Attributes: sender,
			},
			Watcher: callerID,
		},
		Worktree: request.Worktree, NoExecution: true,
	})
}

// modelChoice settles the new root's model and reasoning effort before anything is created.
func (h orchestratorHost) modelChoice(requested, effort string) (string, string, error) {
	model, err := h.spawnModel(requested)
	if err != nil {
		return "", "", err
	}

	level, err := h.s.models.effort(model, effort, "")
	if err != nil {
		return "", "", err
	}

	return model, level, nil
}

func (h orchestratorHost) spawnModel(requested string) (string, error) {
	if requested == "" {
		return h.s.models.defaultModel, nil
	}

	if subagent.IsCandidate(h.s.models.infos, requested) {
		return requested, nil
	}

	var ids []string
	for _, model := range subagent.Candidates(h.s.models.infos) {
		ids = append(ids, model.ID)
	}

	if len(ids) == 0 {
		return "", fmt.Errorf(
			"model %q is not an advertised tagged candidate; no tagged models are configured, so omit model for the daemon's default model",
			requested,
		)
	}

	return "", fmt.Errorf(
		"model %q is not an advertised tagged candidate; choose one of: %s, or omit model for the daemon's default model",
		requested,
		strings.Join(ids, ", "),
	)
}

func (h orchestratorHost) project(ctx context.Context, selection string) (sessionstore.ProjectRow, error) {
	projects, err := h.s.store.ListProjects(ctx)
	if err != nil {
		return sessionstore.ProjectRow{}, fmt.Errorf("list spawn projects: %w", err)
	}

	var (
		matches  []sessionstore.ProjectRow
		known    []string
		rejected error
	)

	for _, project := range projects {
		if project.Hidden {
			continue
		}

		selected := strings.EqualFold(project.Name, selection) || project.WorkDir == selection
		if err := h.projectRefusal(project, selection); err != nil {
			if selected {
				rejected = err
			}

			continue
		}

		known = append(known, project.Name)
		if selected {
			matches = append(matches, project)
		}
	}

	if len(matches) == 1 {
		return matches[0], nil
	}

	if len(matches) > 1 {
		directories := make([]string, 0, len(matches))
		for _, project := range matches {
			directories = append(directories, project.WorkDir)
		}

		slices.Sort(directories)

		return sessionstore.ProjectRow{}, fmt.Errorf(
			"project name %q is ambiguous; use one of: %s", selection, strings.Join(directories, ", "))
	}

	if rejected != nil {
		return sessionstore.ProjectRow{}, rejected
	}

	slices.Sort(known)

	return sessionstore.ProjectRow{}, fmt.Errorf(
		"no project named %q; known projects: %s", selection, strings.Join(known[:min(20, len(known))], ", "))
}

func (h orchestratorHost) projectRefusal(project sessionstore.ProjectRow, selection string) error {
	info, err := os.Stat(project.WorkDir)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf(
			"project %q directory is missing: %s; open an existing project instead", selection, project.WorkDir)
	}

	if err != nil {
		return fmt.Errorf("inspect project directory: %w", err)
	}

	if !info.IsDir() {
		return fmt.Errorf(
			"project %q path is not a directory: %s; open a project directory instead", selection, project.WorkDir)
	}

	if projectpath.IsWorktreePath(projectpath.ResolveWorktreesRoot(h.s.build.Config.UnifiedConfig), project.WorkDir) {
		return fmt.Errorf(
			"project %q is a worktree; name its base project and pass worktree to create a fresh one", selection)
	}

	return nil
}
