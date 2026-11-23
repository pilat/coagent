package localapi

import (
	"context"

	"github.com/pilat/coagent/api/coagent/v1"
)

type chatService struct{}

func (chatService) BindChat(ctx context.Context, req *coagentv1.BindChatRequest) (*coagentv1.BindChatResponse, error) {
	author, err := clientFrom(ctx)
	if err != nil {
		return nil, err
	}

	if err := author.chat.BindChat(ctx, req.GetChatId()); err != nil {
		return nil, refusal(ctx, err)
	}

	return &coagentv1.BindChatResponse{}, nil
}

func (chatService) GetChatFocus(
	ctx context.Context,
	req *coagentv1.GetChatFocusRequest,
) (*coagentv1.GetChatFocusResponse, error) {
	author, err := clientFrom(ctx)
	if err != nil {
		return nil, err
	}

	id, err := author.chat.ChatFocus(ctx, req.GetChatId())
	if err != nil {
		return nil, refusal(ctx, err)
	}

	return &coagentv1.GetChatFocusResponse{SessionId: id}, nil
}

func (chatService) GetChatCursor(
	ctx context.Context,
	req *coagentv1.GetChatCursorRequest,
) (*coagentv1.GetChatCursorResponse, error) {
	author, err := clientFrom(ctx)
	if err != nil {
		return nil, err
	}

	id, cursor, entered, err := author.chat.ChatCursor(ctx, req.GetChatId())
	if err != nil {
		return nil, refusal(ctx, err)
	}

	return &coagentv1.GetChatCursorResponse{SessionId: id, Cursor: cursor, Entered: entered}, nil
}

func (chatService) EnterChat(
	ctx context.Context,
	req *coagentv1.EnterChatRequest,
) (*coagentv1.EnterChatResponse, error) {
	author, err := clientFrom(ctx)
	if err != nil {
		return nil, err
	}

	entered, err := author.chat.EnterChat(ctx, req.GetChatId(), req.GetSessionId(), req.GetNewestId())
	if err != nil {
		return nil, refusal(ctx, err)
	}

	return &coagentv1.EnterChatResponse{Entered: entered}, nil
}

func (chatService) AdvanceChat(
	ctx context.Context,
	req *coagentv1.AdvanceChatRequest,
) (*coagentv1.AdvanceChatResponse, error) {
	author, err := clientFrom(ctx)
	if err != nil {
		return nil, err
	}

	advanced, err := author.chat.AdvanceChat(ctx, req.GetChatId(), req.GetSessionId(), req.GetOutputId())
	if err != nil {
		return nil, refusal(ctx, err)
	}

	return &coagentv1.AdvanceChatResponse{Advanced: advanced}, nil
}

func (chatService) SetChatFocus(
	ctx context.Context,
	req *coagentv1.SetChatFocusRequest,
) (*coagentv1.SetChatFocusResponse, error) {
	author, err := clientFrom(ctx)
	if err != nil {
		return nil, err
	}

	if err := author.chat.SetChatFocus(ctx, req.GetChatId(), req.GetSessionId()); err != nil {
		return nil, refusal(ctx, err)
	}

	return &coagentv1.SetChatFocusResponse{}, nil
}

func (chatService) ResetChatFocus(
	ctx context.Context,
	req *coagentv1.ResetChatFocusRequest,
) (*coagentv1.ResetChatFocusResponse, error) {
	author, err := clientFrom(ctx)
	if err != nil {
		return nil, err
	}

	reset, err := author.chat.ResetChatFocus(ctx, req.GetChatId(), req.GetFromSessionId())
	if err != nil {
		return nil, refusal(ctx, err)
	}

	return &coagentv1.ResetChatFocusResponse{Cleared: reset}, nil
}
