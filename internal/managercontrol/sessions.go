package managercontrol

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionevent"
)

func (s *service) createSession(
	ctx context.Context,
	managerID string,
	data controllerapi.SessionCreateData,
) (int64, error) {
	if err := requireManagerIdentity(managerID); err != nil {
		return 0, err
	}

	if _, supplied := data.Attributes[controllerapi.SessionAttributeOrchestrator]; supplied {
		return 0, errors.New("orchestrator attribute is reserved")
	}

	if _, supplied := data.Attributes[controllerapi.SessionAttributeSpawnedBy]; supplied {
		return 0, errors.New("spawned_by attribute is reserved")
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

	projectID, err := s.resolveSessionProject(ctx, data.WorkDir)
	if err != nil {
		return 0, fmt.Errorf("resolve project: %w", err)
	}

	data.Attributes = maps.Clone(data.Attributes)
	if data.Attributes == nil {
		data.Attributes = make(map[string]any)
	}

	data.Attributes[controllerapi.SessionAttributeManagerID] = managerID

	sessionID, err := s.backend.CreateSession(
		ctx,
		projectID,
		data.WorktreeName,
		data.Prompt,
		data.Model,
		data.Attributes,
	)
	if err != nil {
		// The daemon's text is the refusal shown to the caller.
		return 0, fmt.Errorf("%w", err)
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
	if err := requireManagerIdentity(managerID); err != nil {
		return err
	}

	if _, err := s.managerRoot(ctx, data.SessionID); err != nil {
		return err
	}

	acceptedID, err := s.backend.SendToSessionResolved(ctx, data.SessionID, data.Message, data.IdempotencyKey)
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
	if err := requireManagerIdentity(managerID); err != nil {
		return nil, err
	}

	records, err := s.store.ListSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list sessions: list sessions: %w", err)
	}

	infos := make([]controllerapi.SessionInfo, 0, len(records))
	for _, record := range records {
		if record.ParentID != 0 || !controllerapi.ManagerRoot(record.Attributes) {
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
	if err := requireManagerIdentity(managerID); err != nil {
		return err
	}

	if _, err := s.managerRoot(ctx, data.SessionID); err != nil {
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

	return s.bus.SubscribeManagerRoots()
}

func (s *service) publishInputReceived(sessionID int64, message, source string) {
	s.backend.NotifySession(sessionID, sessionevent.Notification{
		Type: sessionevent.NotifyInputReceived, Message: message, Source: source,
	})
}
