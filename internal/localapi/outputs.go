package localapi

import (
	"context"

	"connectrpc.com/connect/v2"

	"github.com/pilat/coagent/api/coagent/v1"
)

func (outputService) ListOutput(
	ctx context.Context,
	req *coagentv1.ListOutputRequest,
) (*coagentv1.ListOutputResponse, error) {
	author, err := clientFrom(ctx)
	if err != nil {
		return nil, err
	}

	if err := pageLimit(req.GetLimit()); err != nil {
		return nil, err
	}

	rows, err := author.chat.ChatOutputs(ctx, "", req.GetSessionId(), req.GetAfterId(), int(req.GetLimit()))
	if err != nil {
		return nil, refusal(ctx, err)
	}

	return &coagentv1.ListOutputResponse{Rows: outputRows(rows)}, nil
}

func (outputService) ReadEntry(
	ctx context.Context,
	req *coagentv1.ReadEntryRequest,
) (*coagentv1.ReadEntryResponse, error) {
	author, err := clientFrom(ctx)
	if err != nil {
		return nil, err
	}

	if err := pageLimit(req.GetLimit()); err != nil {
		return nil, err
	}

	rows, newest, err := author.chat.ChatEntry(ctx, "", req.GetSessionId(), int(req.GetLimit()))
	if err != nil {
		return nil, refusal(ctx, err)
	}

	return &coagentv1.ReadEntryResponse{Rows: outputRows(rows), NewestId: newest}, nil
}

func pageLimit(limit int32) error {
	if limit < 1 || limit > 100 {
		return connect.Errorf(connect.CodeInvalidArgument, "limit must be 1-100")
	}

	return nil
}
