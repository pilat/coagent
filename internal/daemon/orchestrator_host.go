package daemon

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/orchestrator"
	"github.com/pilat/coagent/internal/projectpath"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/worktree"
)

var _ orchestrator.Host = orchestratorHost{}

type orchestratorHost struct{ s *Service }

func (h orchestratorHost) ListSessions(ctx context.Context, id int64, word string) ([]orchestrator.Entry, int, error) {
	_, owner, err := h.caller(ctx, id)
	if err != nil {
		return nil, 0, err
	}

	rows, err := h.s.store.ChatSessions(ctx, owner)
	if err != nil {
		return nil, 0, fmt.Errorf("list sessions: %w", err)
	}

	rows = slices.DeleteFunc(rows, func(row sessionstore.ChatSession) bool {
		return !strings.Contains(strings.ToLower(row.Name), strings.ToLower(word))
	})
	out := make([]orchestrator.Entry, 0, min(20, len(rows)))

	for _, r := range rows[:min(20, len(rows))] {
		record, err := h.s.store.GetSession(ctx, r.ID)
		if err != nil {
			return nil, 0, fmt.Errorf("load listed session: %w", err)
		}

		state, err := h.sessionState(ctx, record)
		if err != nil {
			return nil, 0, err
		}

		entry := orchestrator.Entry{ID: r.ID, Name: r.Name, State: state, Activity: r.Activity, Self: r.ID == id}
		out = append(out, entry)
	}

	return out, len(rows), nil
}

func (h orchestratorHost) SendToSession(ctx context.Context, callerID, targetID int64, message string) (int64, error) {
	caller, owner, err := h.caller(ctx, callerID)
	if err != nil {
		return 0, err
	}

	target, err := h.sendTarget(ctx, owner, targetID)
	if err != nil {
		return 0, err
	}

	if target.ID == callerID {
		return 0, errors.New("cannot send to this session itself")
	}

	parked, err := h.parked(ctx, target.ID)
	if err != nil {
		return 0, err
	}

	if parked {
		return 0, fmt.Errorf("session #%d is parked at its budget checkpoint; only the human can resume it", target.ID)
	}

	attrs, err := h.provenance(ctx, caller)
	if err != nil {
		return 0, err
	}

	_, err = h.s.store.Enqueue(ctx, sessionstore.Input{
		SessionID: target.ID, Source: sessionstore.InputSourceAgent, Content: message, Attributes: attrs,
	})
	switch {
	case errors.Is(err, sessionstore.ErrSessionNotAcceptingInput):
		return 0, fmt.Errorf("session #%d is stopping; try again shortly", target.ID)
	case err != nil:
		return 0, fmt.Errorf("send to session: %w", err)
	}

	return target.ID, nil
}

func (h orchestratorHost) SpawnSession(ctx context.Context, id int64, name, tree, text string) (int64, string, error) {
	caller, owner, err := h.caller(ctx, id)
	if err != nil {
		return 0, "", err
	}

	if free := h.s.runners.freeSlots(); free < spawnHeadroom {
		const full = "daemon is near its session capacity (%d of %d running); try again after some sessions finish"

		return 0, "", fmt.Errorf(full, maxTotal-free, maxTotal)
	}

	selected, err := h.project(ctx, name)
	if err != nil {
		return 0, "", err
	}

	sender, err := h.provenance(ctx, caller)
	if err != nil {
		return 0, "", err
	}

	attrs := map[string]any{controllerapi.SessionAttributeManagerID: owner}
	if title := controllerapi.SessionTitleFromInput(text); title != "" {
		attrs[controllerapi.SessionAttributeTitle] = title
	}

	var created worktree.Result
	if tree != "" {
		created, err = worktree.Create(ctx, h.s.build.Config.UnifiedConfig, selected.WorkDir, tree, true)
		if err != nil {
			return 0, "", fmt.Errorf("worktree: %w", err)
		}

		selected.WorkDir, selected.Name = created.Path, created.DisplayName

		selected.ID, err = h.s.store.GetOrCreateNamedProject(ctx, selected.WorkDir, selected.Name)
		if err != nil {
			return 0, "", fmt.Errorf("register spawned worktree: %w", created.Abandon(err))
		}

		created.StampAttributes(attrs)
	}

	err = h.s.store.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		id, err = h.createSpawnTx(ctx, tx, selected, text, attrs, sender)

		return err
	})
	if err != nil {
		return 0, "", fmt.Errorf("spawn session: %w", created.Abandon(err))
	}

	return id, selected.Name, nil
}

func (h orchestratorHost) caller(ctx context.Context, id int64) (*sessionstore.SessionRecord, string, error) {
	record, err := h.s.store.GetSession(ctx, id)
	if err == nil && record != nil && record.ParentID == 0 && record.KilledAt == nil &&
		record.Status != sessionstore.SessionStatusKilled && record.Status != sessionstore.SessionStatusTerminating &&
		controllerapi.OrchestratorMode(record.Attributes) {
		if owner, _ := record.Attributes[controllerapi.SessionAttributeManagerID].(string); owner != "" {
			return record, owner, nil
		}
	}

	return nil, "", errors.New("orchestrator mode is off for this session")
}

func (h orchestratorHost) sendTarget(ctx context.Context, owner string, id int64) (*sessionstore.SessionRecord, error) {
	record, err := h.s.store.GetSession(ctx, id)
	if err != nil || record == nil {
		return nil, fmt.Errorf("session #%d not found", id)
	}

	root := record
	if record.ParentID != 0 {
		root, err = h.s.store.GetSession(ctx, record.RootID)
	}

	if err != nil || root == nil || root.Attributes[controllerapi.SessionAttributeManagerID] != owner {
		return nil, fmt.Errorf("session #%d not found", id)
	}

	if record.ParentID != 0 {
		return nil, fmt.Errorf("session #%d is a subagent, not a root session", id)
	}

	resolved, err := h.s.store.ResolveReplacement(ctx, id, owner)
	switch {
	case errors.Is(err, sessionstore.ErrSessionNotAcceptingInput):
		return nil, fmt.Errorf("session #%d is killed", id)
	case err != nil:
		return nil, fmt.Errorf("resolve sent message target: %w", err)
	}

	record, err = h.s.store.GetSession(ctx, resolved)
	if err != nil {
		return nil, fmt.Errorf("reload sent message target: %w", err)
	}

	if record.KilledAt != nil || record.Status == sessionstore.SessionStatusKilled ||
		record.Status == sessionstore.SessionStatusTerminating {
		return nil, fmt.Errorf("session #%d is killed", id)
	}

	return record, nil
}

func (h orchestratorHost) parked(ctx context.Context, id int64) (bool, error) {
	if h.s.parks.active(id) {
		return true, nil
	}

	record, err := h.s.store.Get(ctx, id)
	switch {
	case errors.Is(err, budget.ErrNotFound):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("load sent message budget: %w", err)
	default:
		return record.State == budget.Fired, nil
	}
}

func (h orchestratorHost) sessionState(ctx context.Context, record *sessionstore.SessionRecord) (string, error) {
	if h.s.HasActiveLoop(record.ID) {
		return "running", nil
	}

	parked, err := h.parked(ctx, record.ID)
	if err != nil {
		return "", err
	}

	if parked {
		return "parked", nil
	}

	return cmp.Or(map[sessionstore.SessionStatus]string{
		sessionstore.SessionStatusActive: "idle", sessionstore.SessionStatusCompleted: "idle",
		sessionstore.SessionStatusSuspended: "waiting",
	}[record.Status], string(record.Status)), nil
}

func (h orchestratorHost) provenance(ctx context.Context, from *sessionstore.SessionRecord) (map[string]any, error) {
	project, err := h.s.store.GetProjectName(ctx, from.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("load sender project: %w", err)
	}

	title, _ := from.Attributes[controllerapi.SessionAttributeTitle].(string)

	return map[string]any{
		sessionstore.InputAttributeSenderSessionID: from.ID,
		sessionstore.InputAttributeSenderName:      sessionstore.ChatDisplayName(project, title),
		sessionstore.InputAttributeSenderTitle:     title,
	}, nil
}

func (h orchestratorHost) createSpawnTx(
	ctx context.Context,
	tx *sql.Tx,
	p sessionstore.ProjectRow,
	prompt string,
	attrs, sender map[string]any,
) (int64, error) {
	level, err := h.s.models.effort(h.s.models.defaultModel, "", "")
	if err != nil {
		return 0, err
	}

	record, _, err := sessionstore.CreateManagerRootTx(ctx, tx, sessionstore.ManagerRootCreate{
		ProjectID: p.ID, Name: p.Name, WorkDir: p.WorkDir,
		Model: h.s.models.defaultModel, ReasoningLevel: level, Attributes: attrs,
	})
	if err != nil {
		return 0, fmt.Errorf("create spawned root: %w", err)
	}

	_, err = sessionstore.EnqueueTx(ctx, tx, sessionstore.Input{
		SessionID: record.ID, Source: sessionstore.InputSourceAgent, Content: prompt, Attributes: sender,
	})
	if err != nil {
		return 0, fmt.Errorf("enqueue spawn prompt: %w", err)
	}

	return record.ID, nil
}

func (h orchestratorHost) project(ctx context.Context, name string) (sessionstore.ProjectRow, error) {
	rows, err := h.s.store.ListProjects(ctx)
	if err != nil {
		return sessionstore.ProjectRow{}, fmt.Errorf("list spawn projects: %w", err)
	}
	var match sessionstore.ProjectRow
	var known, dirs []string
	root := projectpath.ResolveRoot(h.s.build.Config.UnifiedConfig)

	for _, row := range rows {
		if row.Hidden || filepath.Dir(row.WorkDir) != root {
			continue
		}

		known = append(known, row.Name)
		if strings.EqualFold(row.Name, name) || row.WorkDir == name {
			match = row
			dirs = append(dirs, row.WorkDir)
		}
	}

	if len(dirs) == 1 {
		return match, nil
	}

	match = sessionstore.ProjectRow{}

	slices.Sort(dirs)

	if len(dirs) > 1 {
		return match, fmt.Errorf("project name %q is ambiguous; use one of: %s", name, strings.Join(dirs, ", "))
	}

	slices.Sort(known)

	return match, fmt.Errorf("no project named %q; known projects: %s", name,
		strings.Join(known[:min(20, len(known))], ", "))
}
