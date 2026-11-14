package daemon

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionstore"
)

type parkRefreshBarrier struct {
	Store
	entered chan struct{}
	release chan struct{}
	held    atomic.Bool
}

type supersededParkBarrier struct {
	budget.Service
	entered chan struct{}
	release chan struct{}
	held    atomic.Bool
}

func TestBudgetParkRetainsScheduledWakeDuringRefresh(t *testing.T) {
	var calls atomic.Int64
	h := newHarness(t, harnessOptions{respond: func(string, []llmwire.Message) *llmwire.Response {
		calls.Add(1)
		return textReply("unexpected paid call")
	}})
	root := h.createRoot(managerAttrs(scenarioManagerID))
	fireHandoffBudget(t, h, root, "first")
	barrier := &parkRefreshBarrier{Store: h.mgr.store, entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { closeOnce(barrier.release) })
	h.mgr.store = barrier
	h.mgr.startPark(root)
	waitForScenarioSignal(t, barrier.entered, "park refresh after unlocking the stopped tree")
	require.Equal(t, sessionstore.SessionStatusStopped, h.session(root).Status)
	h.startInboxWake()
	applied, err := enqueueScheduledInput(h.ctx, h.schedules, root, "scheduled wake", false)
	require.NoError(t, err)
	require.True(t, applied)
	h.waitUntil("scheduled run ends on the fired budget", func() bool {
		return h.session(root).Status == sessionstore.SessionStatusSuspended && !h.mgr.HasActiveLoop(root)
	})
	closeOnce(barrier.release)
	h.waitUntil("scheduled root is parked again", func() bool {
		return h.session(root).Status == sessionstore.SessionStatusStopped && !h.mgr.parks.active(root)
	})
	assert.Zero(t, calls.Load())
	record, err := h.store.Get(h.ctx, root)
	require.NoError(t, err)
	assert.Equal(t, budget.Fired, record.State)
}

func TestBudgetParkRetainsFireWhileSupersededParkExits(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: trivialRespond})
	root := h.createRoot(managerAttrs(scenarioManagerID))
	fireHandoffBudget(t, h, root, "first")
	limit := 2.0
	armed, err := h.store.Arm(h.ctx, budget.Mutation{
		RootSessionID: root, ToolCallID: "rearm", CostLimitUSD: &limit, Receipt: "Budget rearmed",
	})
	require.NoError(t, err)
	barrier := &supersededParkBarrier{
		Service: h.mgr.budgets,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	t.Cleanup(func() { closeOnce(barrier.release) })
	h.mgr.budgets = barrier
	h.mgr.startPark(root)
	waitForScenarioSignal(t, barrier.entered, "park reads the superseding armed generation")
	_, _, err = h.store.FireBudget(h.ctx, root, armed.Generation, "cost", 3, "second checkpoint")
	require.NoError(t, err)
	require.NoError(t, h.mgr.settleRunBudget(h.ctx, h.session(root), session.RunResult{BudgetFired: true}, nil))
	closeOnce(barrier.release)
	h.waitUntil("new fired generation is parked", func() bool {
		return h.session(root).Status == sessionstore.SessionStatusStopped && !h.mgr.parks.active(root)
	})
	record, err := h.store.Get(h.ctx, root)
	require.NoError(t, err)
	assert.Equal(t, budget.Fired, record.State)
	assert.Equal(t, armed.Generation, record.Generation)
}

func (b *parkRefreshBarrier) Get(ctx context.Context, rootID int64) (*budget.Record, error) {
	record, err := b.Store.Get(ctx, rootID)
	if err == nil && record.State == budget.Fired && b.held.CompareAndSwap(false, true) {
		close(b.entered)
		<-b.release
	}
	return record, err
}

func (b *supersededParkBarrier) Get(ctx context.Context, rootID int64) (*budget.Record, error) {
	record, err := b.Service.Get(ctx, rootID)
	if err == nil && record.State == budget.Armed && b.held.CompareAndSwap(false, true) {
		close(b.entered)
		<-b.release
	}
	return record, err
}

func fireHandoffBudget(t *testing.T, h *harness, rootID int64, key string) {
	t.Helper()
	limit := 1.0
	armed, err := h.store.Arm(h.ctx, budget.Mutation{
		RootSessionID: rootID, ToolCallID: key, CostLimitUSD: &limit, Receipt: "Budget armed",
	})
	require.NoError(t, err)
	_, _, err = h.store.FireBudget(h.ctx, rootID, armed.Generation, "cost", 1.5, "checkpoint")
	require.NoError(t, err)
}
