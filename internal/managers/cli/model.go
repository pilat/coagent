package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/ctl"
)

type (
	// ModelsResult is the model catalog plus the current choice for one chat.
	ModelsResult struct {
		Models        []controllerapi.ConfigModelInfo `json:"models"`
		CurrentID     string                          `json:"current_id,omitempty"`
		CurrentEffort string                          `json:"current_effort,omitempty"`
	}

	// SetModelParams changes the model and reasoning effort of one chat session.
	SetModelParams struct {
		SessionID      int64  `json:"session_id"`
		Model          string `json:"model"`
		ReasoningLevel string `json:"reasoning_level,omitempty"`
	}
)

func (m *Manager) modelsHandler() ctl.Handler {
	return func(ctx context.Context, _ *ctl.Conn, params json.RawMessage) (any, *ctl.Error) {
		var p SessionParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &ctl.Error{Code: ctl.CodeInvalidParams, Message: err.Error()}
		}

		res, err := m.models(ctx, p.SessionID)
		if err != nil {
			return nil, &ctl.Error{Code: ctl.CodeInternal, Message: err.Error()}
		}

		return res, nil
	}
}

func (m *Manager) setModelHandler() ctl.Handler {
	return func(ctx context.Context, _ *ctl.Conn, params json.RawMessage) (any, *ctl.Error) {
		var p SetModelParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &ctl.Error{Code: ctl.CodeInvalidParams, Message: err.Error()}
		}

		if p.SessionID == 0 || p.Model == "" {
			return nil, &ctl.Error{Code: ctl.CodeInvalidParams, Message: "session and model are required"}
		}

		if err := m.requireOwnedSession(p.SessionID); err != nil {
			return nil, &ctl.Error{Code: ctl.CodeInvalidParams, Message: err.Error()}
		}

		err := m.controller.SetSessionModel(ctx, controllerapi.SessionSetModelData(p))
		if err != nil {
			return nil, &ctl.Error{Code: ctl.CodeInternal, Message: err.Error()}
		}

		return SendResult{SessionID: p.SessionID}, nil
	}
}

func (m *Manager) models(ctx context.Context, sessionID int64) (ModelsResult, error) {
	if sessionID != 0 {
		if err := m.requireOwnedSession(sessionID); err != nil {
			return ModelsResult{}, err
		}
	}

	catalog, err := m.controller.ListModels(ctx)
	if err != nil {
		return ModelsResult{}, fmt.Errorf("list chat models: %w", err)
	}

	if catalog == nil {
		return ModelsResult{}, errors.New("model catalog is unavailable")
	}

	result := ModelsResult{Models: catalog.Models, CurrentID: m.model}
	if sessionID == 0 {
		return result, nil
	}

	sessions, err := m.controller.ListSessions(ctx)
	if err != nil {
		return ModelsResult{}, fmt.Errorf("list chat sessions: %w", err)
	}

	for _, session := range sessions {
		if session.ID == sessionID {
			result.CurrentID = session.Model
			result.CurrentEffort = session.ReasoningLevel

			break
		}
	}

	return result, nil
}
