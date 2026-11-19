package daemon

import (
	"context"
	"database/sql"
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
	"github.com/pilat/coagent/internal/worktree"
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

	request.Model, err = h.spawnModel(request.Model)
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

	attrs := map[string]any{controllerapi.SessionAttributeManagerID: owner}
	if title := controllerapi.SessionTitleFromInput(request.Prompt); title != "" {
		attrs[controllerapi.SessionAttributeTitle] = title
	}

	var created worktree.Result
	if request.Worktree != "" {
		created, err = worktree.Create(ctx, h.s.build.Config.UnifiedConfig, selected.WorkDir, request.Worktree, true)
		if err != nil {
			return 0, "", fmt.Errorf("worktree: %w", err)
		}

		selected.WorkDir = created.Path
		selected.Name = created.DisplayName

		selected.ID, err = h.s.store.GetOrCreateNamedProject(ctx, selected.WorkDir, selected.Name)
		if err != nil {
			return 0, "", fmt.Errorf("register spawned worktree: %w", created.Abandon(err))
		}

		created.StampAttributes(attrs)
	}

	var sessionID int64

	err = h.s.store.WithTx(ctx, func(tx *sql.Tx) error {
		var err error

		sessionID, err = h.createSpawnTx(ctx, tx, selected, request, attrs, sender)
		if err != nil {
			return err
		}

		_, err = sessionstore.ArmWatchTx(ctx, tx, callerID, sessionID, false)
		if err != nil {
			return fmt.Errorf("arm spawned session watch: %w", err)
		}

		return nil
	})
	if err != nil {
		return 0, "", fmt.Errorf("spawn session: %w", created.Abandon(err))
	}

	return sessionID, selected.Name, nil
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

func (h orchestratorHost) createSpawnTx(
	ctx context.Context,
	tx *sql.Tx,
	project sessionstore.ProjectRow,
	request orchestrator.SpawnRequest,
	attrs, sender map[string]any,
) (int64, error) {
	level, err := h.s.models.effort(request.Model, "", "")
	if err != nil {
		return 0, err
	}

	record, _, err := sessionstore.CreateManagerRootTx(ctx, tx, sessionstore.ManagerRootCreate{
		ProjectID:      project.ID,
		Name:           project.Name,
		WorkDir:        project.WorkDir,
		Model:          request.Model,
		ReasoningLevel: level,
		Attributes:     attrs,
	})
	if err != nil {
		return 0, fmt.Errorf("create spawned root: %w", err)
	}

	_, err = sessionstore.EnqueueTx(ctx, tx, sessionstore.Input{
		SessionID:  record.ID,
		Source:     sessionstore.InputSourceAgent,
		Content:    request.Prompt,
		Attributes: sender,
	})
	if err != nil {
		return 0, fmt.Errorf("enqueue spawn prompt: %w", err)
	}

	return record.ID, nil
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
