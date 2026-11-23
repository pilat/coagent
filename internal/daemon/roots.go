package daemon

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/worktree"
)

// rootRequest carries the differences between a human creation and an orchestrator spawn.
type rootRequest struct {
	Root        sessionstore.ManagerRootCreate
	Worktree    string // Forks Root's project into a fresh worktree the root runs in.
	NoExecution bool
	Start       bool // Launches the runner at once; a spawned root is woken by its first input instead.
}

// CreateSession creates and starts a root, optionally in a fresh worktree.
// A manager-owned root commits its opened row and first input with the session.
func (s *Service) CreateSession(
	ctx context.Context,
	projectID int64,
	worktreeName, prompt, model string,
	attrs map[string]any,
) (int64, error) {
	workDir, err := s.store.GetProjectWorkDir(ctx, projectID)
	if err != nil {
		return 0, fmt.Errorf("resolve project %d: %w", projectID, err)
	}

	if model == "" {
		model = s.models.defaultModel
	}

	level, err := s.models.effort(model, "", "")
	if err != nil {
		return 0, fmt.Errorf("resolve reasoning level for model %s: %w", model, err)
	}

	owner, _ := attrs[controllerapi.SessionAttributeManagerID].(string)
	if owner != "" {
		projectName, err := s.store.GetProjectName(ctx, projectID)
		if err != nil {
			return 0, fmt.Errorf("resolve project name: %w", err)
		}

		id, _, err := s.createManagerRoot(ctx, rootRequest{
			Root: sessionstore.ManagerRootCreate{
				ProjectID: projectID, Name: projectName, WorkDir: workDir,
				Model: model, ReasoningLevel: level, Attributes: attrs,
				Input: sessionstore.Input{Source: sessionstore.InputSourceUser, Content: prompt},
			},
			Worktree: worktreeName, Start: true,
		})

		return id, err
	}

	record, err := s.store.CreateSession(ctx, projectID, model, level, attrs)
	if err != nil {
		return 0, fmt.Errorf("create session record: %w", err)
	}

	if prompt != "" {
		if _, err := s.store.Enqueue(ctx, sessionstore.Input{
			SessionID: record.ID, Source: sessionstore.InputSourceUser, Content: prompt,
		}); err != nil {
			return 0, fmt.Errorf("persist initial session input: %w", err)
		}
	}

	if err := s.startCreated(ctx, record.ID); err != nil {
		return 0, err
	}

	return record.ID, nil
}

func (s *Service) createManagerRoot(ctx context.Context, request rootRequest) (int64, string, error) {
	root := request.Root

	created, err := s.forkRootWorktree(ctx, &root, request)
	if err != nil {
		return 0, "", err
	}

	record, _, err := s.store.CreateManagerRoot(ctx, root)
	if err != nil {
		return 0, "", fmt.Errorf("create manager root: %w", created.Abandon(err))
	}

	if request.Start {
		if err := s.startCreated(ctx, record.ID); err != nil {
			return 0, "", fmt.Errorf("%w", created.Abandon(err))
		}
	}

	return record.ID, root.Name, nil
}

func (s *Service) forkRootWorktree(
	ctx context.Context,
	root *sessionstore.ManagerRootCreate,
	request rootRequest,
) (worktree.Result, error) {
	if request.Worktree == "" {
		return worktree.Result{}, nil
	}

	created, err := worktree.Create(
		ctx,
		s.build.Config.UnifiedConfig,
		root.WorkDir,
		request.Worktree,
		request.NoExecution,
	)
	if err != nil {
		return worktree.Result{}, fmt.Errorf("worktree: %w", err)
	}

	projectID, err := s.store.GetOrCreateNamedProject(ctx, created.Path, created.DisplayName)
	if err != nil {
		return worktree.Result{}, fmt.Errorf("register worktree project: %w", created.Abandon(err))
	}

	root.ProjectID = projectID
	root.Name = created.DisplayName
	root.WorkDir = created.Path
	created.StampAttributes(root.Attributes)

	return created, nil
}

// startCreated kills a new root whose launch fails, so no live record outlives a failed create.
func (s *Service) startCreated(ctx context.Context, sessionID int64) error {
	if err := s.start(ctx, sessionID); err != nil {
		// The kill marker must persist even if the request was cancelled.
		if _, killErr := s.store.MarkSessionKilledWithOutput(
			context.WithoutCancel(ctx),
			sessionID,
			0,
		); killErr != nil {
			logger.Ctx(ctx).Named("daemon.manager").Warn("cleanup_orphaned_session",
				zap.Int64("session_id", sessionID), zap.Error(killErr))
		}

		return err
	}

	return nil
}
