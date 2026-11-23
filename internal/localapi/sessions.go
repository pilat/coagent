package localapi

import (
	"context"
	"path/filepath"
	"strings"

	"connectrpc.com/connect/v2"

	"github.com/pilat/coagent/api/coagent/v1"
	"github.com/pilat/coagent/internal/controllerapi"
)

const maxIdempotencyKeyBytes = 128

type sessionService struct {
	// Accepted sends must outlive client cancellation but end with the server.
	closing <-chan struct{}
}

func (sessionService) CreateSession(
	ctx context.Context,
	req *coagentv1.CreateSessionRequest,
) (*coagentv1.CreateSessionResponse, error) {
	author, err := clientFrom(ctx)
	if err != nil {
		return nil, err
	}

	if !filepath.IsAbs(req.GetWorkDir()) {
		return nil, connect.Errorf(connect.CodeInvalidArgument, "work_dir must be an absolute path")
	}

	id, err := author.control.CreateSession(ctx, controllerapi.SessionCreateData{
		WorkDir: req.GetWorkDir(), Model: req.GetModel(),
	})
	if err != nil {
		return nil, refusal(ctx, err)
	}

	return &coagentv1.CreateSessionResponse{SessionId: id}, nil
}

func (s sessionService) SendInput(
	ctx context.Context,
	req *coagentv1.SendInputRequest,
) (*coagentv1.SendInputResponse, error) {
	author, err := clientFrom(ctx)
	if err != nil {
		return nil, err
	}

	if req.GetSessionId() <= 0 {
		return nil, connect.Errorf(connect.CodeInvalidArgument, "session_id must be positive")
	}

	if strings.TrimSpace(req.GetText()) == "" {
		return nil, connect.Errorf(connect.CodeInvalidArgument, "text must not be blank")
	}

	if req.GetIdempotencyKey() == "" || len(req.GetIdempotencyKey()) > maxIdempotencyKeyBytes {
		return nil, connect.Errorf(connect.CodeInvalidArgument, "idempotency_key must be 1-128 bytes")
	}

	ctx, cancel := s.sendContext(ctx)
	defer cancel()

	err = author.control.SendSessionMessage(ctx, controllerapi.SessionMessageData{
		SessionID: req.GetSessionId(), Message: req.GetText(), IdempotencyKey: req.GetIdempotencyKey(),
	})
	if err != nil {
		return nil, refusal(ctx, err)
	}

	return &coagentv1.SendInputResponse{}, nil
}

func refusal(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return connect.Errorf(connect.CodeUnavailable, "the local API is closing")
	}

	return connect.Errorf(connect.CodeFailedPrecondition, "%s", err.Error())
}
