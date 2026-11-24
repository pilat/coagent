package telegram

import (
	"context"
	"testing"

	"connectrpc.com/connect/v2"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/localapi"
)

var (
	_ controllerapi.Controller               = servedController{}
	_ controllerapi.ChatFocusController      = servedController{}
	_ controllerapi.LineageController        = servedController{}
	_ controllerapi.ManagerControllerFactory = servedControllers{}
)

type servedController struct {
	controllerapi.Controller
	controllerapi.ChatFocusController
}

type servedControllers struct {
	control servedController
}

func (c servedController) Successor(context.Context, int64) (int64, error) {
	return 0, nil
}

func (c servedController) SendSessionMessage(ctx context.Context, data controllerapi.SessionMessageData) error {
	data.IdempotencyKey = ""
	return c.Controller.SendSessionMessage(ctx, data)
}

func (f servedControllers) ForManager(string) controllerapi.Controller {
	return f.control
}

func (f servedControllers) ForApp(string, []string) controllerapi.Controller {
	return f.control
}

func servedTransport(
	t *testing.T,
	control controllerapi.Controller,
	chat controllerapi.ChatFocusController,
) connect.Transport {
	t.Helper()
	transport, err := localapi.NewInProcess(t.Context(), servedControllers{
		control: servedController{Controller: control, ChatFocusController: chat},
	}).Transport("telegram-main")
	require.NoError(t, err)
	return transport
}

func servedAPI(t *testing.T, control controllerapi.Controller, chat controllerapi.ChatFocusController) apiClients {
	t.Helper()
	return newAPI(servedTransport(t, control, chat))
}
