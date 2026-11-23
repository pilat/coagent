package telegram

import (
	"context"
	"errors"

	"connectrpc.com/connect/v2"

	"github.com/pilat/coagent/api/coagent/v1/coagentv1connect"
)

var _ connect.ClientStream = refusalStream{}

type apiClients struct {
	sessions  coagentv1connect.SessionServiceClient
	chats     coagentv1connect.ChatServiceClient
	outputs   coagentv1connect.OutputServiceClient
	discovery coagentv1connect.DiscoveryServiceClient
}

type refusalStream struct {
	connect.ClientStream
}

func (s refusalStream) Receive(message any) error {
	return asRefusal(s.ClientStream.Receive(message))
}

func newAPI(transport connect.Transport) apiClients {
	client := connect.NewClient(transport, refusalText)

	return apiClients{
		sessions:  coagentv1connect.NewSessionServiceClient(client),
		chats:     coagentv1connect.NewChatServiceClient(client),
		outputs:   coagentv1connect.NewOutputServiceClient(client),
		discovery: coagentv1connect.NewDiscoveryServiceClient(client),
	}
}

// refusalText preserves the controller's refusal text shown to the user.
func refusalText(next connect.ClientFunc) connect.ClientFunc {
	return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
		stream, err := next(ctx, spec)
		if err != nil {
			return nil, asRefusal(err)
		}

		return refusalStream{ClientStream: stream}, nil
	}
}

func asRefusal(err error) error {
	if refused, ok := errors.AsType[*connect.Error](err); ok && refused.Message() != "" {
		return errors.New(refused.Message())
	}

	return err
}
