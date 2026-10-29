package daemon

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

func TestBackgroundObligationProjectsTreeLedgersAndInbox(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: trivialRespond})
	root := h.createRoot(nil)
	child := h.createUnlinkedChild(root)
	obligation, err := h.mgr.store.HasBackgroundObligationByRoot(h.ctx, root)
	require.NoError(t, err)
	assert.False(t, obligation)
	h.attachChildLink(subagent.Link{
		ParentID: root, ChildID: child, TaskCallID: "background", Blocking: false,
		State: subagent.StateRunning,
	})
	obligation, err = h.mgr.store.HasBackgroundObligationByRoot(h.ctx, root)
	require.NoError(t, err)
	assert.True(t, obligation)
	budgetProbe := &budgetServiceProbe{record: &budget.Record{State: budget.Armed, Generation: 1}}
	h.mgr.budgets = budgetProbe
	retained, err := h.mgr.retainBudgetForBackground(h.ctx, root)
	require.NoError(t, err)
	assert.True(t, retained)
	assert.Zero(t, budgetProbe.releaseCalls)
	other := h.createRoot(nil)
	_, err = h.store.Enqueue(
		h.ctx, sessionstore.Input{
			SessionID:  other,
			Source:     sessionstore.InputSourceProcess,
			Content:    "other",
			Attributes: nil,
		},
	)
	require.NoError(t, err)
	obligation, err = h.mgr.store.HasBackgroundObligationByRoot(h.ctx, other)
	require.NoError(t, err)
	assert.True(t, obligation)

	// A stopped or killed link promises no wake: it neither bypasses the
	// completion check nor retains the budget on that promise (D4/D5).
	for _, state := range []subagent.State{subagent.StateStopped, subagent.StateKilled} {
		h.createChild(root, subagent.Link{TaskCallID: "background-" + string(state), Blocking: false, State: state})
	}
	otherRoot := h.createRoot(nil)
	for _, state := range []subagent.State{subagent.StateStopped, subagent.StateKilled} {
		h.createChild(otherRoot, subagent.Link{
			TaskCallID: "background-" + string(state), Blocking: false, State: state,
		})
	}
	obligation, err = h.mgr.store.HasBackgroundObligationByRoot(h.ctx, otherRoot)
	require.NoError(t, err)
	assert.False(t, obligation)
	retained, err = h.mgr.retainBudgetForBackground(h.ctx, otherRoot)
	require.NoError(t, err)
	assert.False(t, retained)
}

func TestReleaseArmedBudgetHonorsStateAndErrors(t *testing.T) {
	t.Parallel()
	t.Run("released budget is unchanged", func(t *testing.T) {
		service := &budgetServiceProbe{record: &budget.Record{State: budget.Released}}
		manager := &svc{budgets: service}
		require.NoError(t, manager.releaseArmedBudget(t.Context(), 1, "stopped"))
		assert.Zero(t, service.releaseCalls)
	})
	t.Run("armed budget is released", func(t *testing.T) {
		service := &budgetServiceProbe{record: &budget.Record{State: budget.Armed, Generation: 3}}
		manager := &svc{budgets: service}
		require.NoError(t, manager.releaseArmedBudget(t.Context(), 1, "stopped"))
		assert.Equal(t, 1, service.releaseCalls)
	})
	t.Run("release error is preserved", func(t *testing.T) {
		releaseErr := errors.New("release failed")
		service := &budgetServiceProbe{record: &budget.Record{State: budget.Armed}, releaseErr: releaseErr}
		manager := &svc{budgets: service}
		err := manager.releaseArmedBudget(t.Context(), 1, "stopped")
		require.ErrorIs(t, err, releaseErr)
		assert.Equal(t, 1, service.releaseCalls)
	})
	t.Run("superseded budget completes with an info log", func(t *testing.T) {
		core, logs := observer.New(zap.InfoLevel)
		ctx := logger.ToContext(t.Context(), zap.New(core))
		service := &budgetServiceProbe{
			record:     &budget.Record{State: budget.Armed, Generation: 3},
			releaseErr: fmt.Errorf("stale release: %w", budget.ErrConflict),
		}
		manager := &svc{budgets: service}
		require.NoError(t, manager.releaseArmedBudget(ctx, 12, "completed"))
		assert.Equal(t, 1, service.releaseCalls)
		entries := logs.FilterMessage("terminal_budget_release_superseded").All()
		require.Len(t, entries, 1)
		assert.Equal(t, zap.InfoLevel, entries[0].Level)
		assert.Equal(t, int64(12), entries[0].ContextMap()["session_id"])
		assert.Equal(t, int64(3), entries[0].ContextMap()["generation"])
	})
}

var errBudgetParkProbe = errors.New("budget park probe")

type budgetServiceProbe struct {
	budget.Service
	record       *budget.Record
	getErr       error
	releaseErr   error
	releaseCalls int
	beginCalls   chan struct{}
}

func (s *budgetServiceProbe) Get(context.Context, int64) (*budget.Record, error) {
	return s.record, s.getErr
}

func (s *budgetServiceProbe) Release(
	context.Context,
	int64,
	int64,
	string,
) (*budget.Record, error) {
	s.releaseCalls++
	return s.record, s.releaseErr
}

func (s *budgetServiceProbe) BeginDrain(
	context.Context,
	int64,
	int64,
	string,
) (*budget.Record, error) {
	s.beginCalls <- struct{}{}
	return nil, errBudgetParkProbe
}
