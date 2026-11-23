package localapi

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
)

type lineageFactory func(string) controllerapi.Controller

type lineageGate struct {
	controllerapi.Controller
	controllerapi.ChatFocusController
	controllerapi.LineageController
	after func(context.Context, int64)
}

func (f lineageFactory) ForManager(id string) controllerapi.Controller {
	return f(id)
}

func (g *lineageGate) Successor(ctx context.Context, id int64) (int64, error) {
	next, err := g.LineageController.Successor(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("read gated successor: %w", err)
	}
	g.after(ctx, next)
	return next, nil
}

func gateSuccessor(t *testing.T, f *apiFixture, after func(context.Context, int64)) {
	t.Helper()
	factory := f.factory
	f.factory = lineageFactory(func(id string) controllerapi.Controller {
		control := factory.ForManager(id)
		chat, ok := control.(controllerapi.ChatFocusController)
		require.True(t, ok)
		lineage, ok := control.(controllerapi.LineageController)
		require.True(t, ok)
		return &lineageGate{
			Controller: control, ChatFocusController: chat, LineageController: lineage, after: after,
		}
	})
}
