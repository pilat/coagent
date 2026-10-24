package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/assistant"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/projectpath"
	"github.com/pilat/coagent/internal/sessionbuild"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
)

var _ assistant.Host = assistantHost{}

// assistantHost gives assistant roots the daemon lifecycle for work roots.
type assistantHost struct {
	s *svc
}

// EnsureAssistantRoot reuses the hidden management project without a management surface marker.
func (s *svc) EnsureAssistantRoot(ctx context.Context, owner string) (*sessionstore.SessionRecord, error) {
	path := filepath.Join(
		projectpath.ResolveRoot(s.build.Config.UnifiedConfig),
		controllerapi.CoagentManagementProjectDir,
	)
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, fmt.Errorf("create assistant project: %w", err)
	}

	project, err := s.store.GetOrCreateHiddenProject(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("resolve assistant project: %w", err)
	}

	record, err := s.store.EnsureAssistantRoot(ctx, project, owner, path)
	if err != nil {
		return nil, fmt.Errorf("ensure assistant root: %w", err)
	}

	return record, nil
}

func (h assistantHost) NewClient(id int64, model, level string) (llm.Client, error) {
	if model == "" {
		model = h.s.build.Config.DefaultModel()
	}

	cfg := *h.s.build.Config
	unified := *cfg.UnifiedConfig
	disabled := false
	unified.Tools.Search.Enabled = &disabled
	cfg.UnifiedConfig = &unified

	client, _, err := sessionbuild.BuildClient(&cfg, model, level)
	if err != nil {
		return nil, fmt.Errorf("build assistant client: %w", err)
	}

	client.SetSessionID(strconv.FormatInt(id, 10))

	return client, nil
}

func (h assistantHost) CreateWork(
	ctx context.Context,
	owner string,
	projectID int64,
	name, workDir string,
) (int64, error) {
	level, err := h.s.models.effort(h.s.models.defaultModel, "", "")
	if err != nil {
		return 0, err
	}

	record, _, err := h.s.store.CreateManagerRoot(ctx, sessionstore.ManagerRootCreate{
		ProjectID: projectID, Model: h.s.models.defaultModel, ReasoningLevel: level,
		Attributes: map[string]any{controllerapi.SessionAttributeManagerID: owner},
		Name:       name, WorkDir: workDir,
	})
	if err != nil {
		return 0, fmt.Errorf("create assistant work session: %w", err)
	}

	return record.ID, nil
}

func (h assistantHost) Deliver(ctx context.Context, id int64, text string, enqueue func(*sql.Tx) error) error {
	unlock, err := h.s.lockSessionTree(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()

	if err := h.s.store.WithTx(ctx, enqueue); err != nil {
		return fmt.Errorf("commit assistant instruction: %w", err)
	}

	// The instruction is already durable; boot recovery starts it after a shutdown.
	if err := h.s.sendToSessionLocked(ctx, id, text); err != nil && !errors.Is(err, errDaemonShuttingDown) {
		return err
	}

	return nil
}

func (h assistantHost) Stop(ctx context.Context, id int64) error {
	return h.s.sendToSession(ctx, id, stopCommand)
}

func (h assistantHost) Notify(id int64, text string) {
	h.s.publish(id, sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: text})
}

func (s *svc) runAssistant(ctx context.Context, rs *runner) runOutcome {
	rs.MarkRun()

	// The explicit conversion keeps arch-lint's deep scan from reading a daemon type as an assistant dependency.
	if err := assistant.Run(ctx, s.assistant, assistant.Host(assistantHost{s: s}), rs.sessionID); err != nil {
		logger.Ctx(ctx).Named("daemon.assistant").Error("run assistant", zap.Error(err))
		return runOutcome{errored: true}
	}

	return runOutcome{hadInput: true}
}

// refuseAssistantRoot resolves a kill or clear aimed at an assistant root with the loop's own ignore receipt.
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

func (s *svc) sweepAssistantObservations(ctx context.Context) error {
	ids, err := s.assistant.AwaitedSessions(ctx)
	if err != nil {
		return fmt.Errorf("list awaited assistant sessions: %w", err)
	}

	for _, id := range ids {
		unlock, err := s.lockSessionTree(ctx, id)
		if err != nil {
			return err
		}

		if !s.HasActiveLoop(id) {
			err = s.assistant.Observe(ctx, id, nil)
		}

		unlock()

		if err != nil {
			return fmt.Errorf("observe assistant work %d: %w", id, err)
		}
	}

	return nil
}
