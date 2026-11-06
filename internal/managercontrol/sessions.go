package managercontrol

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/worktree"
)

func (s *service) createSession(
	ctx context.Context,
	managerID string,
	data controllerapi.SessionCreateData,
) (int64, error) {
	if err := requireManagerIdentity(managerID); err != nil {
		return 0, err
	}

	if data.RepoRoot != "" {
		return 0, errors.New("repo root is reserved for created worktrees")
	}

	if _, supplied := data.Attributes[controllerapi.SessionAttributeTitle]; supplied {
		return 0, errors.New("title attribute is reserved")
	}

	if _, supplied := data.Attributes["repo_root"]; supplied {
		return 0, errors.New("repo_root attribute is reserved for created worktrees")
	}

	if _, supplied := data.Attributes[controllerapi.SessionAttributeWorktreeOrigin]; supplied {
		return 0, errors.New("worktree origin is reserved for created worktrees")
	}

	var (
		created             worktree.Result
		worktreeProjectName string
	)

	if data.WorktreeName != "" {
		next, err := worktree.Create(ctx, s.unifiedConfig(), data.WorkDir, data.WorktreeName, false)
		if err != nil {
			return 0, fmt.Errorf("worktree: %w", err)
		}

		data.WorkDir = next.Path
		// Consumed: the created worktree is registered below under its display name.
		data.WorktreeName = ""
		data.RepoRoot = next.RepoRoot
		worktreeProjectName = next.DisplayName
		created = next
	}

	data.Attributes = maps.Clone(data.Attributes)
	if data.Attributes == nil {
		data.Attributes = make(map[string]any)
	}

	data.Attributes[controllerapi.SessionAttributeManagerID] = managerID

	created.StampAttributes(data.Attributes)

	projectID, err := s.resolveSessionProject(ctx, data, worktreeProjectName)
	if err != nil {
		return 0, fmt.Errorf("resolve project: %w", created.Abandon(err))
	}

	sessionID, err := s.backend.Send(ctx, projectID, data.Prompt, data.Model, data.Attributes)
	if err != nil {
		return 0, fmt.Errorf("send session: %w", created.Abandon(err))
	}

	if data.Prompt != "" {
		s.publishInputReceived(sessionID, data.Prompt, "user")
	}

	return sessionID, nil
}

func (s *service) sendSessionMessage(
	ctx context.Context,
	managerID string,
	data controllerapi.SessionMessageData,
) error {
	if err := s.requireOwnedSession(ctx, managerID, data.SessionID); err != nil {
		return err
	}

	acceptedID, err := s.backend.SendToSessionResolved(ctx, data.SessionID, data.Message)
	if err != nil {
		return fmt.Errorf("send resolved session message: %w", err)
	}

	if data.Message != "" {
		s.publishInputReceived(acceptedID, data.Message, "user")
	}

	return nil
}

func (s *service) listSessions(
	ctx context.Context,
	managerID string,
) ([]controllerapi.SessionInfo, error) {
	records, err := s.store.ListSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list sessions: list sessions: %w", err)
	}

	infos := make([]controllerapi.SessionInfo, 0, len(records))
	for _, record := range records {
		if !s.canListSession(ctx, managerID, record) {
			continue
		}

		workDir, _ := s.store.GetProjectWorkDir(ctx, record.ProjectID)
		projectName, _ := s.store.GetProjectName(ctx, record.ProjectID)
		infos = append(infos, controllerapi.SessionInfo{
			ID: record.ID, Name: fmt.Sprintf("%s - %d", projectName, record.ID),
			WorkDir: workDir, ProjectID: record.ProjectID, ProjectName: projectName,
			HasActiveLoop: s.backend.HasActiveLoop(record.ID), Model: record.Model,
			ReasoningLevel: record.ReasoningLevel, Status: string(record.Status),
			Attributes: record.Attributes, UpdatedAt: record.UpdatedAt, KilledAt: record.KilledAt,
		})
	}

	return infos, nil
}

func (s *service) setSessionModel(
	ctx context.Context,
	managerID string,
	data controllerapi.SessionSetModelData,
) error {
	if err := s.requireOwnedSession(ctx, managerID, data.SessionID); err != nil {
		return err
	}

	if err := s.backend.SetModel(ctx, data.SessionID, data.Model, data.ReasoningLevel); err != nil {
		return fmt.Errorf("set session model: %w", err)
	}

	return nil
}

func (s *service) subscribe(managerID string) <-chan controllerapi.SessionNotification {
	if managerID == "" {
		return make(chan controllerapi.SessionNotification)
	}

	return s.bus.SubscribeManager(managerID)
}

func (s *service) publishInputReceived(sessionID int64, message, source string) {
	s.backend.NotifySession(sessionID, sessionevent.Notification{
		Type: sessionevent.NotifyInputReceived, Message: message, Source: source,
	})
}
