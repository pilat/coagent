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

	if data.SystemProject != "" && managerID != controllerapi.BuiltinCLIManagerID {
		return 0, errors.New("the reserved system project belongs to the local chat")
	}

	if data.SystemProject != "" && data.SystemProject != controllerapi.CoagentSystemProjectName {
		return 0, fmt.Errorf("unknown system project %q", data.SystemProject)
	}

	if data.SystemProject != "" && data.UseWorktree {
		return 0, fmt.Errorf("system project %q cannot use a worktree", data.SystemProject)
	}

	if data.UseWorktree {
		nextWorkDir, err := createWorktree(ctx, data.WorkDir)
		if err != nil {
			return 0, err
		}

		data.WorkDir = nextWorkDir
	}

	data.Attributes = maps.Clone(data.Attributes)
	if data.Attributes == nil {
		data.Attributes = make(map[string]any)
	}

	data.Attributes[controllerapi.SessionAttributeManagerID] = managerID

	projectID, err := s.resolveSessionProject(ctx, data)
	if err != nil {
		return 0, fmt.Errorf("resolve project: %w", err)
	}

	sessionID, err := s.backend.Send(ctx, projectID, data.Prompt, data.Model, data.Attributes)
	if err != nil {
		return 0, fmt.Errorf("send session: %w", err)
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
	if err := s.requireOwnedSession(ctx, managerID, data.SessionID); err != nil {
		return 0, err
	}

	acceptedID, err := s.backend.SendToSessionResolved(ctx, data.SessionID, data.Message)
	if err != nil {
		return 0, fmt.Errorf("send resolved session message: %w", err)
	}

	if data.Message != "" {
		s.publishInputReceived(acceptedID, data.Message, "user")
	}

	return acceptedID, nil
}

func (s *service) listSessions(
	ctx context.Context,
	managerID string,
) ([]controllerapi.SessionInfo, error) {
	records, err := s.backend.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}

	infos := make([]controllerapi.SessionInfo, 0, len(records))
	for _, record := range records {
		if !s.canListSession(ctx, managerID, record) {
			continue
		}

		workDir, _ := s.backend.GetProjectWorkDir(ctx, record.ProjectID)
		projectName, _ := s.backend.GetProjectName(ctx, record.ProjectID)
		infos = append(infos, controllerapi.SessionInfo{
			ID: record.ID, Name: fmt.Sprintf("%s - %d", projectName, record.ID),
			WorkDir: workDir, ProjectID: record.ProjectID,
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

func (s *service) setSessionAttributes(
	ctx context.Context,
	managerID string,
	data controllerapi.SessionSetAttributesData,
) error {
	if err := s.authorizeAttributeUpdate(ctx, managerID, &data); err != nil {
		return err
	}

	if err := s.backend.SetAttributes(ctx, data.SessionID, data.Attributes); err != nil {
		return fmt.Errorf("set session attributes: %w", err)
	}

	return nil
}

func (s *service) currentProgress(
	ctx context.Context,
	managerID string,
	sessionID int64,
) (*controllerapi.ProgressData, error) {
	if err := s.requireOwnedSession(ctx, managerID, sessionID); err != nil {
		return nil, err
	}

	return s.backend.CurrentProgress(ctx, sessionID) //nolint:wrapcheck // Backend supplies operation context.
}

func (s *service) refreshProgress(ctx context.Context, managerID string, sessionID int64) error {
	if err := s.requireOwnedSession(ctx, managerID, sessionID); err != nil {
		return err
	}

	return s.backend.RefreshProgress(ctx, sessionID) //nolint:wrapcheck // Backend supplies operation context.
}

func (s *service) subscribe(managerID string) <-chan controllerapi.SessionNotification {
	if managerID == "" {
		return make(chan controllerapi.SessionNotification)
	}

	return s.backend.PubSub().SubscribeManager(managerID)
}

func (s *service) unsubscribe(ch <-chan controllerapi.SessionNotification) {
	s.backend.PubSub().UnsubscribeManager(ch)
}

func (s *service) publishInputReceived(sessionID int64, message, source string) {
	s.backend.NotifySession(sessionID, sessionevent.Notification{
		Type: sessionevent.NotifyInputReceived, Message: message, Source: source,
	})
}
