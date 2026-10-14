package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pilat/coagent/internal/assistant"
	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/projectpath"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

func (a *assistantSessions) Open(ctx context.Context, project, text string) (assistant.Card, error) {
	id, path, name, err := a.resolveProject(ctx, project)
	if err != nil {
		return assistant.Card{}, err
	}

	s := a.daemon

	level, err := s.models.effort(s.models.defaultModel, "", "")
	if err != nil {
		return assistant.Card{}, err
	}

	record, _, err := s.store.CreateManagerRoot(ctx, sessionstore.ManagerRootCreate{
		ProjectID: id, Model: s.models.defaultModel, ReasoningLevel: level,
		Attributes: map[string]any{controllerapi.SessionAttributeManagerID: a.owner}, Name: name, WorkDir: path,
	})
	if err != nil {
		return assistant.Card{}, fmt.Errorf("create assistant work session: %w", err)
	}

	if err := a.Send(ctx, record.ID, text); err != nil {
		return assistant.Card{}, err
	}

	return assistant.Card{ID: record.ID, Project: name, Status: assistantWorking, Activity: time.Now().UTC()}, nil
}

func (a *assistantSessions) Send(ctx context.Context, id int64, text string) error {
	s := a.daemon

	unlock, err := s.lockSessionTree(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()

	if _, err := a.workRoot(ctx, id); err != nil {
		return err
	}

	if err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		in, err := sessionstore.EnqueueTx(ctx, tx, sessionstore.Input{
			SessionID:  id,
			Source:     sessionstore.InputSourceUser,
			Content:    text,
			Attributes: map[string]any{"from": "assistant"},
		})
		if err != nil {
			return fmt.Errorf("enqueue assistant instruction: %w", err)
		}

		return assistant.RecordSendTx(ctx, tx, assistant.Send{
			InputID: in.Input.ID, AssistantSessionID: a.root,
			SessionID: id, CreatedAt: time.Now().UTC(),
		})
	}); err != nil {
		return fmt.Errorf("commit assistant instruction: %w", err)
	}

	return s.sendToSessionLocked(ctx, id, text)
}

func (a *assistantSessions) Stop(ctx context.Context, id int64) error {
	if _, err := a.workRoot(ctx, id); err != nil {
		return err
	}

	sends, err := a.daemon.assistantStore.UnobservedSends(ctx, id)
	if err != nil {
		return fmt.Errorf("list stopped assistant sends: %w", err)
	}

	if err := a.daemon.store.WithTx(ctx, func(tx *sql.Tx) error {
		for _, send := range sends {
			if _, err := assistant.MarkObservedTx(ctx, tx, send.InputID, time.Now().UTC()); err != nil {
				return fmt.Errorf("settle stopped assistant send: %w", err)
			}
		}

		return nil
	}); err != nil {
		return fmt.Errorf("settle assistant stop: %w", err)
	}

	return a.daemon.sendToSession(ctx, id, stopCommand)
}

func (a *assistantSessions) workRoot(ctx context.Context, id int64) (*sessionstore.SessionRecord, error) {
	record, err := a.daemon.store.GetSession(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("load assistant target: %w", err)
	}

	owner, _ := record.Attributes[controllerapi.SessionAttributeManagerID].(string)
	if owner != a.owner || record.ParentID != 0 || record.AgentType == tool.AssistantAgentType ||
		isManagementSurface(record.Attributes) || record.KilledAt != nil {
		return nil, errors.New("assistant target is not an owned work root")
	}

	return record, nil
}

func (a *assistantSessions) resolveProject(ctx context.Context, raw string) (int64, string, string, error) {
	name := strings.TrimSpace(raw)

	projects, err := a.daemon.store.ListProjects(ctx)
	if err != nil {
		return 0, "", "", fmt.Errorf("list assistant projects: %w", err)
	}

	if path, ok := projectPathArg(name); ok {
		for _, project := range projects {
			if project.WorkDir == path && !project.Hidden {
				return project.ID, project.WorkDir, project.Name, nil
			}
		}

		return 0, "", "", fmt.Errorf("no project at %s", raw)
	}

	var existing *sessionstore.ProjectRow

	for i := range projects {
		if projects[i].Name == name && !projects[i].Hidden {
			if existing != nil {
				return 0, "", "", fmt.Errorf("project name %q is ambiguous; open it by its path from the board", name)
			}

			existing = &projects[i]
		}
	}

	if existing != nil {
		return existing.ID, existing.WorkDir, existing.Name, nil
	}

	name, err = projectpath.SanitizeName(name)
	if err != nil {
		return 0, "", "", fmt.Errorf("resolve assistant project: %w", err)
	}

	path := filepath.Join(projectpath.ResolveRoot(a.daemon.build.Config.UnifiedConfig), name)
	if err := os.MkdirAll(path, 0o755); err != nil {
		return 0, "", "", fmt.Errorf("create assistant work directory: %w", err)
	}

	id, err := a.daemon.store.GetOrCreateProject(ctx, path)
	if err != nil {
		return 0, "", "", fmt.Errorf("create assistant work project: %w", err)
	}

	return id, path, name, nil
}

// projectPathArg accepts the path a board card shows, so a name shared by several
// projects can still be opened.
func projectPathArg(raw string) (string, bool) {
	if strings.HasPrefix(raw, "~/") {
		home, err := coagenthome.UserHome()
		if err != nil {
			return "", false
		}

		return filepath.Join(home, raw[2:]), true
	}

	if filepath.IsAbs(raw) {
		return filepath.Clean(raw), true
	}

	return "", false
}
