package localapi

import (
	"context"

	"github.com/pilat/coagent/api/coagent/v1"
	"github.com/pilat/coagent/internal/controllerapi"
)

func (sessionService) ListSessions(
	ctx context.Context,
	_ *coagentv1.ListSessionsRequest,
) (*coagentv1.ListSessionsResponse, error) {
	author, err := clientFrom(ctx)
	if err != nil {
		return nil, err
	}

	sessions, err := author.chat.ChatSessions(ctx, "")
	if err != nil {
		return nil, refusal(ctx, err)
	}

	response := &coagentv1.ListSessionsResponse{}
	for _, session := range sessions {
		response.Sessions = append(response.Sessions, sessionSummary(session))
	}

	return response, nil
}

func (sessionService) ListSessionDetails(
	ctx context.Context,
	_ *coagentv1.ListSessionDetailsRequest,
) (*coagentv1.ListSessionDetailsResponse, error) {
	author, err := clientFrom(ctx)
	if err != nil {
		return nil, err
	}

	sessions, err := author.control.ListSessions(ctx)
	if err != nil {
		return nil, refusal(ctx, err)
	}

	response := &coagentv1.ListSessionDetailsResponse{}
	for _, session := range sessions {
		response.Sessions = append(response.Sessions, sessionDetails(session))
	}

	return response, nil
}

func (sessionService) GetSessionKilled(
	ctx context.Context,
	req *coagentv1.GetSessionKilledRequest,
) (*coagentv1.GetSessionKilledResponse, error) {
	author, err := clientFrom(ctx)
	if err != nil {
		return nil, err
	}

	killed, err := author.chat.SessionKilled(ctx, req.GetSessionId())
	if err != nil {
		return nil, refusal(ctx, err)
	}

	return &coagentv1.GetSessionKilledResponse{Killed: killed}, nil
}

func (sessionService) GetOrchestratorMode(
	ctx context.Context,
	req *coagentv1.GetOrchestratorModeRequest,
) (*coagentv1.GetOrchestratorModeResponse, error) {
	author, err := clientFrom(ctx)
	if err != nil {
		return nil, err
	}

	on, err := author.chat.Orchestrator(ctx, "", req.GetSessionId())
	if err != nil {
		return nil, refusal(ctx, err)
	}

	return &coagentv1.GetOrchestratorModeResponse{On: on}, nil
}

func sessionSummary(session controllerapi.ChatSessionInfo) *coagentv1.SessionSummary {
	return &coagentv1.SessionSummary{Id: session.ID, Name: session.Name, Scheduled: session.Scheduled}
}

func sessionDetails(session controllerapi.SessionInfo) *coagentv1.SessionDetails {
	return &coagentv1.SessionDetails{
		Id: session.ID, WorkDir: session.WorkDir, Model: session.Model, Effort: session.ReasoningLevel,
		Status: session.Status, Killed: session.KilledAt != nil,
	}
}
