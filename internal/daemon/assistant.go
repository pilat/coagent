package daemon

import (
	"context"
	"database/sql"
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

var _ assistant.Sessions = (*assistantSessions)(nil)

type assistantSessions struct {
	daemon *svc
	root   int64
	owner  string
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

func (s *svc) runAssistant(ctx context.Context, rs *runner) runOutcome {
	deps, err := s.assistantDeps(ctx, rs.sessionID)
	if err != nil {
		logger.Ctx(ctx).Named("daemon.assistant").Error("prepare assistant", zap.Error(err))
		return runOutcome{errored: true}
	}

	rs.MarkRun()

	if err := assistant.Run(ctx, deps, rs.sessionID); err != nil {
		logger.Ctx(ctx).Named("daemon.assistant").Error("run assistant", zap.Error(err))
		return runOutcome{errored: true}
	}

	return runOutcome{hadInput: true}
}

func (s *svc) assistantDeps(ctx context.Context, id int64) (assistant.Deps, error) {
	record, err := s.store.GetSession(ctx, id)
	if err != nil {
		return assistant.Deps{}, fmt.Errorf("load assistant: %w", err)
	}

	owner, _ := record.Attributes[controllerapi.SessionAttributeManagerID].(string)
	var message string

	return assistant.Deps{
		Store: s.store, Memory: s.assistantStore,
		Sessions: &assistantSessions{daemon: s, root: id, owner: owner},
		Now:      func() time.Time { return time.Now().UTC() },
		NewClient: func(model string) (llm.Client, error) {
			if model == "" {
				model = s.build.Config.DefaultModel()
			}

			current, err := s.store.GetSession(ctx, id)
			if err != nil {
				return nil, fmt.Errorf("load assistant reasoning level: %w", err)
			}

			cfg := *s.build.Config
			unified := *cfg.UnifiedConfig
			disabled := false
			unified.Tools.Search.Enabled = &disabled
			cfg.UnifiedConfig = &unified

			client, _, err := sessionbuild.BuildClient(&cfg, model, current.ReasoningLevel)
			if err != nil {
				return nil, fmt.Errorf("build assistant client: %w", err)
			}

			client.SetSessionID(strconv.FormatInt(id, 10))

			return client, nil
		},
		SayTx: func(ctx context.Context, tx *sql.Tx, text string) error {
			if err := sessionstore.PublishAssistantMessageTx(ctx, tx, id, text); err != nil {
				return fmt.Errorf("publish assistant reply: %w", err)
			}

			message = text

			return nil
		},
		Notify: func() { s.publish(id, sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: message}) },
		Enqueue: func(ctx context.Context, kind string, target int64, text string) error {
			_, err := s.store.Enqueue(ctx, assistantEventInput(id, target, kind, text))
			if err != nil {
				return fmt.Errorf("enqueue assistant event: %w", err)
			}

			return nil
		},
	}, nil
}

func assistantEventInput(id, target int64, kind, text string) sessionstore.Input {
	return sessionstore.Input{
		SessionID: id, Source: sessionstore.InputSourceAgent, Content: text,
		Attributes: map[string]any{"assistant_event": kind, "session_id": target},
	}
}
