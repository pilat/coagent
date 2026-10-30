package daemon

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/pilat/coagent/internal/assistant"
	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/configapply"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/projectpath"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

var _ assistant.Host = assistantHost{}

// assistantHost gives assistant roots the daemon lifecycle for work roots.
type assistantHost struct {
	s *svc
}

// EnsureAssistantRoot preserves each manager's assistant identity in the shared hidden project.
func (s *svc) EnsureAssistantRoot(ctx context.Context, owner string) (*sessionstore.SessionRecord, error) {
	path := filepath.Join(
		projectpath.ResolveRoot(s.build.Config.UnifiedConfig),
		controllerapi.AssistantProjectDir,
	)
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, fmt.Errorf("create assistant project: %w", err)
	}

	project, err := s.store.GetOrCreateHiddenProject(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("resolve assistant project: %w", err)
	}

	level, err := s.models.effort(s.models.defaultModel, "", "")
	if err != nil {
		return nil, err
	}

	record, err := s.store.EnsureAssistantRoot(ctx, project, owner, path, s.models.defaultModel, level)
	if err != nil {
		return nil, fmt.Errorf("ensure assistant root: %w", err)
	}

	return record, nil
}

func (h assistantHost) CreateWork(
	ctx context.Context,
	owner string,
	projectID int64,
	name, workDir, title string,
) (int64, error) {
	level, err := h.s.models.effort(h.s.models.defaultModel, "", "")
	if err != nil {
		return 0, err
	}

	record, _, err := h.s.store.CreateManagerRoot(ctx, sessionstore.ManagerRootCreate{
		ProjectID: projectID, Model: h.s.models.defaultModel, ReasoningLevel: level,
		Attributes: map[string]any{
			controllerapi.SessionAttributeManagerID: owner,
			controllerapi.SessionAttributeTitle:     title,
		},
		Name: name, WorkDir: workDir,
	})
	if err != nil {
		return 0, fmt.Errorf("create assistant work session: %w", err)
	}

	return record.ID, nil
}

func (h assistantHost) Deliver(
	ctx context.Context,
	id int64,
	text string,
	enqueue func(*sql.Tx, map[string]any) (*sessionstore.InboxInput, error),
) error {
	unlock, err := h.s.lockSessionTree(ctx, id)
	if err != nil {
		return err
	}
	var input *sessionstore.InboxInput
	command := false

	err = func() error {
		defer unlock()

		attributes, err := h.s.sessionInputAttributes(ctx, id, text)
		if err != nil {
			return err
		}

		if err := h.s.store.WithTx(ctx, func(tx *sql.Tx) error {
			var enqueueErr error
			input, enqueueErr = enqueue(tx, attributes)

			return enqueueErr
		}); err != nil {
			return fmt.Errorf("commit assistant instruction: %w", err)
		}

		command = input != nil && isEnqueueCommand(text)
		if command {
			return nil
		}

		// The instruction is already durable; boot recovery starts it after a shutdown.
		if err := h.s.sendToSessionLocked(ctx, id, text); err != nil && !errors.Is(err, errDaemonShuttingDown) {
			return err
		}

		return nil
	}()
	if err != nil || !command {
		return err
	}

	_, err = h.s.handleGenericCommand(ctx, input)

	return err
}

func (h assistantHost) Stop(ctx context.Context, id int64) error {
	return h.s.sendToSession(ctx, id, stopCommand)
}

func (h assistantHost) FocusChat(ctx context.Context, owner string, id int64) error {
	return h.s.store.FocusOwnerChat(ctx, owner, id) //nolint:wrapcheck // Preserve chat refusal text.
}

func (h assistantHost) Models(context.Context) ([]assistant.Model, error) {
	result := make([]assistant.Model, 0, len(h.s.models.entries))
	for _, entry := range h.s.models.entries {
		model := assistant.Model{
			ID: entry.ID, DisplayName: cmp.Or(entry.DisplayName, entry.Name, entry.ID),
			EffortLevels: entry.EffortLevels, DefaultEffort: entry.DefaultEffort,
			Reasoning: entry.Reasoning != nil && entry.Reasoning.Supported,
		}
		if entry.Pricing != nil {
			model.Priced, model.InputPrice, model.OutputPrice = true, entry.Pricing.InputPrice, entry.Pricing.OutputPrice
		}

		result = append(result, model)
	}

	return result, nil
}

func (h assistantHost) SetModel(ctx context.Context, id int64, model, effort string) error {
	return h.s.SetModel(ctx, id, model, effort)
}

func (s *svc) assistantHead(scope assistant.Scope) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		now := time.Now().UTC()

		desk, err := s.assistant.LoadDesk(ctx, scope.Root, now)
		if err != nil {
			return "", fmt.Errorf("load assistant desk: %w", err)
		}

		recent, total, err := s.assistant.Recent(ctx, scope, assistant.RecentSessions)
		if err != nil {
			return "", fmt.Errorf("load assistant recent sessions: %w", err)
		}

		return assistant.Render(assistant.View{Desk: desk, Recent: recent, Total: total}, now), nil
	}
}

func (s *svc) assistantAdministrationTools(rec *sessionstore.SessionRecord, scope assistant.Scope) []tool.Tool {
	tools := s.administrationTools(rec, 0)
	for i, instance := range tools {
		if instance.ID() == budget.ToolID {
			tools[i] = budget.NewTargetedTool(budget.Store(s.store), rec.ID, s.assistantBudgetTarget(scope))
		}
	}

	return append(tools, configapply.NewConfigEdit(rec.ID, s.applier), configapply.NewConfigRead(rec.ID, s.applier))
}

func (s *svc) assistantBudgetTarget(scope assistant.Scope) func(context.Context, int64) (int64, bool, error) {
	return func(ctx context.Context, id int64) (int64, bool, error) {
		if id == 0 {
			id = scope.Root
		} else if _, err := s.assistant.Target(ctx, scope, id); err != nil {
			return 0, false, err //nolint:wrapcheck // Preserve target refusal text.
		}

		record, err := s.store.GetSession(ctx, id)
		if err != nil {
			return 0, false, fmt.Errorf("load budget target: %w", err)
		}

		return id, s.models.priced(record.Model), nil
	}
}

// refuseAssistantRoot keeps refused lifecycle commands out of the model transcript.
func (s *svc) refuseAssistantRoot(ctx context.Context, input *sessionstore.InboxInput) (bool, error) {
	record, err := s.store.GetSession(ctx, input.SessionID)
	if err != nil {
		return false, fmt.Errorf("load command target: %w", err)
	}

	if record.AgentType != sessionstore.AssistantAgentType {
		return false, nil
	}

	err = s.store.WithTx(ctx, func(tx *sql.Tx) error {
		return sessionstore.HandleAssistantInputTx(ctx, tx, record.ID, input.ID,
			"assistant_ignored_command", time.Now().UTC())
	})
	if err != nil && !errors.Is(err, sessionstore.ErrInputResolved) {
		return true, fmt.Errorf("ignore assistant command: %w", err)
	}

	return true, nil
}
