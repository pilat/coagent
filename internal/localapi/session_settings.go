package localapi

import (
	"context"

	"github.com/pilat/coagent/api/coagent/v1"
	"github.com/pilat/coagent/internal/controllerapi"
)

func (sessionService) SetSessionModel(
	ctx context.Context,
	req *coagentv1.SetSessionModelRequest,
) (*coagentv1.SetSessionModelResponse, error) {
	author, err := clientFrom(ctx)
	if err != nil {
		return nil, err
	}

	err = author.control.SetSessionModel(ctx, controllerapi.SessionSetModelData{
		SessionID: req.GetSessionId(), Model: req.GetModel(), ReasoningLevel: req.GetEffort(),
	})
	if err != nil {
		return nil, refusal(ctx, err)
	}

	return &coagentv1.SetSessionModelResponse{}, nil
}

func (sessionService) SetSessionTitle(
	ctx context.Context,
	req *coagentv1.SetSessionTitleRequest,
) (*coagentv1.SetSessionTitleResponse, error) {
	author, err := clientFrom(ctx)
	if err != nil {
		return nil, err
	}

	if err := author.chat.SetSessionTitle(ctx, "", req.GetSessionId(), req.GetTitle()); err != nil {
		return nil, refusal(ctx, err)
	}

	return &coagentv1.SetSessionTitleResponse{}, nil
}

func (sessionService) SetOrchestratorMode(
	ctx context.Context,
	req *coagentv1.SetOrchestratorModeRequest,
) (*coagentv1.SetOrchestratorModeResponse, error) {
	author, err := clientFrom(ctx)
	if err != nil {
		return nil, err
	}

	changed, err := author.chat.SetOrchestrator(ctx, "", req.GetSessionId(), req.GetOn())
	if err != nil {
		return nil, refusal(ctx, err)
	}

	return &coagentv1.SetOrchestratorModeResponse{Changed: changed}, nil
}
