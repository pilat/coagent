package localapi

import (
	"context"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectinprocess"

	"github.com/pilat/coagent/internal/controllerapi"
)

// InProcess binds configured manager ids to transports with no socket or token.
type InProcess struct {
	controllers controllerapi.ManagerControllerFactory
	closing     <-chan struct{}
}

// NewInProcess bounds accepted sends by the daemon's lifetime.
func NewInProcess(ctx context.Context, controllers controllerapi.ManagerControllerFactory) *InProcess {
	return &InProcess{controllers: controllers, closing: ctx.Done()}
}

// Transport authenticates calls as the manager id supplied by configuration.
func (p *InProcess) Transport(managerID string) (connect.Transport, error) {
	author, err := bindAuthor(managerID, p.controllers)
	if err != nil {
		return nil, err
	}

	server := connect.NewServer(stampAuthor(author))
	register(server, p.closing)

	return connectinprocess.New(server), nil
}

func stampAuthor(author *client) connect.ServerInterceptor {
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			return next(context.WithValue(ctx, clientKey{}, author), spec, stream)
		}
	}
}
