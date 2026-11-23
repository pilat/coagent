package telegram

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/api/coagent/v1"
	"github.com/pilat/coagent/internal/controllerapi"
)

type resubscribingController struct {
	*fakeController
	subscriptions atomic.Int64
}

func (c *resubscribingController) Subscribe() <-chan controllerapi.SessionNotification {
	c.subscriptions.Add(1)
	return c.fakeController.Subscribe()
}

func TestNotificationsLoop_ResubscribesWhenTheStreamEnds(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fake := &fakeController{}
		var messages []string
		manager := newCmdTestManager(t, fake, &messages)
		wrapper := &resubscribingController{fakeController: fake}
		manager.api = servedAPI(t, wrapper, fake)
		loop := &chatLoopStub{}
		manager.delivery = loop
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		watch, err := manager.api.outputs.WatchSessions(ctx, &coagentv1.WatchSessionsRequest{})
		require.NoError(t, err)
		go manager.notificationsLoop(ctx, watch)
		time.Sleep(4 * time.Second)
		synctest.Wait()
		assert.GreaterOrEqual(t, wrapper.subscriptions.Load(), int64(2))
		assert.GreaterOrEqual(t, loop.wakes, 2)
		cancel()
		synctest.Wait()
	})
}
