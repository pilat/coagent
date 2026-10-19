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
)

// projectChoice is a resolved open target; id zero means a new scratch project at path.
type projectChoice struct {
	id   int64
	path string
	name string
}

func (a *assistantSessions) CheckProject(ctx context.Context, project string) error {
	_, err := a.lookupProject(ctx, project)
	return err
}

func (a *assistantSessions) Open(ctx context.Context, project, text, about string) (assistant.Card, error) {
	choice, err := a.resolveProject(ctx, project)
	if err != nil {
		return assistant.Card{}, err
	}

	s := a.daemon

	level, err := s.models.effort(s.models.defaultModel, "", "")
	if err != nil {
		return assistant.Card{}, err
	}

	record, _, err := s.store.CreateManagerRoot(ctx, sessionstore.ManagerRootCreate{
		ProjectID: choice.id, Model: s.models.defaultModel, ReasoningLevel: level,
		Attributes: map[string]any{controllerapi.SessionAttributeManagerID: a.owner},
		Name:       choice.name, WorkDir: choice.path,
	})
	if err != nil {
		return assistant.Card{}, fmt.Errorf("create assistant work session: %w", err)
	}

	if err := a.send(ctx, record.ID, text, about); err != nil {
		return assistant.Card{}, err
	}

	now := time.Now().UTC()

	return assistant.Card{
		ID: record.ID, Project: choice.name, Status: assistantWorking, CreatedAt: now, Activity: now, About: about,
	}, nil
}

func (a *assistantSessions) Send(ctx context.Context, id int64, text string) error {
	return a.send(ctx, id, text, "")
}

func (a *assistantSessions) send(ctx context.Context, id int64, text, about string) error {
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

		now := time.Now().UTC()
		if err := assistant.RecordSendTx(ctx, tx, assistant.Send{
			InputID: in.Input.ID, AssistantSessionID: a.root, SessionID: id, CreatedAt: now,
		}); err != nil {
			return fmt.Errorf("record assistant send: %w", err)
		}

		if about != "" {
			return assistant.SaveAboutTx(ctx, tx, id, about, now)
		}

		return nil
	}); err != nil {
		return fmt.Errorf("commit assistant instruction: %w", err)
	}

	// The instruction is already durable; boot recovery starts it after a shutdown.
	if err := s.sendToSessionLocked(ctx, id, text); err != nil && !errors.Is(err, errDaemonShuttingDown) {
		return err
	}

	return nil
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

// workRoot applies the assistant store's authority predicate, the same one search and listings use.
func (a *assistantSessions) workRoot(ctx context.Context, id int64) (*sessionstore.SessionRecord, error) {
	record, err := a.daemon.store.GetSession(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("load assistant target: %w", err)
	}

	ok, err := a.daemon.assistantStore.Authorized(ctx, a.owner, id)
	if err != nil {
		return nil, fmt.Errorf("authorize assistant target: %w", err)
	}

	if !ok {
		return nil, fmt.Errorf("session %d is not a work session you can address", id)
	}

	return record, nil
}

func (a *assistantSessions) resolveProject(ctx context.Context, raw string) (projectChoice, error) {
	choice, err := a.lookupProject(ctx, raw)
	if err != nil || choice.id != 0 {
		return choice, err
	}

	if err := os.MkdirAll(choice.path, 0o755); err != nil {
		return projectChoice{}, fmt.Errorf("create assistant work directory: %w", err)
	}

	choice.id, err = a.daemon.store.GetOrCreateProject(ctx, choice.path)
	if err != nil {
		return projectChoice{}, fmt.Errorf("create assistant work project: %w", err)
	}

	return choice, nil
}

// lookupProject never creates anything, so open can be validated long before it runs.
func (a *assistantSessions) lookupProject(ctx context.Context, raw string) (projectChoice, error) {
	name := strings.TrimSpace(raw)

	projects, err := a.daemon.store.ListProjects(ctx)
	if err != nil {
		return projectChoice{}, fmt.Errorf("list assistant projects: %w", err)
	}

	if path, ok := projectPathArg(name); ok {
		for _, project := range projects {
			if project.WorkDir == path && !project.Hidden {
				return projectChoice{id: project.ID, path: project.WorkDir, name: project.Name}, nil
			}
		}

		return projectChoice{}, fmt.Errorf("no project at %s", raw)
	}

	var existing *sessionstore.ProjectRow

	for i := range projects {
		if projects[i].Name == name && !projects[i].Hidden {
			if existing != nil {
				return projectChoice{}, fmt.Errorf(
					"project name %q is ambiguous; open it by its path from projects",
					name,
				)
			}

			existing = &projects[i]
		}
	}

	if existing != nil {
		return projectChoice{id: existing.ID, path: existing.WorkDir, name: existing.Name}, nil
	}

	name, err = projectpath.SanitizeName(name)
	if err != nil {
		return projectChoice{}, fmt.Errorf("resolve assistant project: %w", err)
	}

	path := filepath.Join(projectpath.ResolveRoot(a.daemon.build.Config.UnifiedConfig), name)

	return projectChoice{path: path, name: name}, nil
}

// projectPathArg accepts the catalog's path key, so a name shared by several
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
