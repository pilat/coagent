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

	if err := reservedAttributes(data.Attributes); err != nil {
		return 0, err
	}

	projectID, err := s.resolveSessionProject(ctx, data.WorkDir)
	if err != nil {
		return 0, fmt.Errorf("resolve project: %w", err)
	}

	data.Attributes = maps.Clone(data.Attributes)
	if data.Attributes == nil {
		data.Attributes = make(map[string]any)
	}

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
) (int64, error) {
	if err := requireManagerIdentity(managerID); err != nil {
		return 0, err
	}

	if _, err := s.requireRoot(ctx, data.SessionID); err != nil {
		return 0, err
	}

	acceptedID, err := s.backend.SendToSessionResolved(
		ctx,
		data.SessionID,
		data.Message,
		authorKey(managerID, data.IdempotencyKey),
	)
	if err != nil {
		return 0, fmt.Errorf("send resolved session message: %w", err)
	}

	if data.Message != "" {
		s.publishInputReceived(acceptedID, data.Message, "user")
	}

	return acceptedID, nil
}

func authorKey(author, key string) string {
	if key == "" {
		return ""
	}

	return author + ":" + key
}

func reservedAttributes(attrs map[string]any) error {
	for _, key := range []string{
		controllerapi.SessionAttributeOrchestrator, controllerapi.SessionAttributeSpawnedBy,
		controllerapi.SessionAttributeSpawnedByApp, controllerapi.SessionAttributeTitle,
	} {
		if _, supplied := attrs[key]; supplied {
			return fmt.Errorf("%s attribute is reserved", key)
		}
	}

	if _, supplied := attrs["repo_root"]; supplied {
		return errors.New("repo_root attribute is reserved for created worktrees")
	}

	if _, supplied := attrs[controllerapi.SessionAttributeWorktreeOrigin]; supplied {
		return errors.New("worktree origin is reserved for created worktrees")
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
		if record.ParentID != 0 {
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

	if _, err := s.requireRoot(ctx, data.SessionID); err != nil {
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

	return s.bus.SubscribeRoots()
}

func (s *service) publishInputReceived(sessionID int64, message, source string) {
	s.backend.NotifySession(sessionID, sessionevent.Notification{
		Type: sessionevent.NotifyInputReceived, Message: message, Source: source,
	})
}
