package daemon

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

func TestBackgroundObligationProjectsTreeLedgersAndInbox(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: trivialRespond})
	defer h.shutdown()

	root, err := h.store.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)
	child, err := func() (int64, error) {
		var id int64
		err := h.store.WithTx(h.ctx, func(tx *sql.Tx) error {
			var err error
			id, err = sessionstore.CreateSubagentSessionTx(
				h.ctx,
				tx,
				sessionstore.CreateSubagentSession{
					ProjectID:      h.projectID,
					ParentID:       root.ID,
					RootID:         root.ID,
					AgentType:      "general",
					Model:          "fake-model",
					ReasoningLevel: "",
				},
			)
			return err
		})
		return id, err
	}()
	require.NoError(t, err)

	obligation, err := h.mgr.store.HasBackgroundObligationByRoot(h.ctx, root.ID)
	require.NoError(t, err)
	assert.False(t, obligation)

	require.NoError(t, seedChildLink(h.ctx, h.store, subagent.Link{
		ParentID: root.ID, ChildID: child, TaskCallID: "background", Blocking: false,
		State: subagent.StateRunning,
	}))
	obligation, err = h.mgr.store.HasBackgroundObligationByRoot(h.ctx, root.ID)
	require.NoError(t, err)
	assert.True(t, obligation)
	budgetProbe := &budgetServiceProbe{record: &budget.Record{
		State: budget.Armed, Generation: 1,
	}}
	h.mgr.budgets = budgetProbe
	retained, err := h.mgr.retainBudgetForBackground(h.ctx, root.ID)
	require.NoError(t, err)
	assert.True(t, retained)
	assert.Zero(t, budgetProbe.releaseCalls)

	other, err := h.store.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)
	_, err = h.store.Enqueue(
		h.ctx,
		sessionstore.Input{
			SessionID:  other.ID,
			Source:     sessionstore.InputSourceProcess,
			Content:    "other",
			Attributes: nil,
		},
	)
	require.NoError(t, err)
	obligation, err = h.mgr.store.HasBackgroundObligationByRoot(h.ctx, other.ID)
	require.NoError(t, err)
	assert.True(t, obligation)

	// A stopped or killed link promises no wake: it neither bypasses the
	// completion check nor retains the budget on that promise (D4/D5).
	for _, state := range []subagent.State{subagent.StateStopped, subagent.StateKilled} {
		stoppedChild, err := func() (int64, error) {
			var id int64
			err := h.store.WithTx(h.ctx, func(tx *sql.Tx) error {
				var err error
				id, err = sessionstore.CreateSubagentSessionTx(
					h.ctx,
					tx,
					sessionstore.CreateSubagentSession{
						ProjectID:      h.projectID,
						ParentID:       root.ID,
						RootID:         root.ID,
						AgentType:      "general",
						Model:          "fake-model",
						ReasoningLevel: "",
					},
				)
				return err
			})
			return id, err
		}()
		require.NoError(t, err)
		require.NoError(t, seedChildLink(h.ctx, h.store, subagent.Link{
			ParentID: root.ID, ChildID: stoppedChild,
			TaskCallID: "background-" + string(state), Blocking: false, State: state,
		}))
	}

	otherRoot, err := h.store.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)
	for _, state := range []subagent.State{subagent.StateStopped, subagent.StateKilled} {
		otherChild, err := func() (int64, error) {
			var id int64
			err := h.store.WithTx(h.ctx, func(tx *sql.Tx) error {
				var err error
				id, err = sessionstore.CreateSubagentSessionTx(
					h.ctx,
					tx,
					sessionstore.CreateSubagentSession{
						ProjectID:      h.projectID,
						ParentID:       otherRoot.ID,
						RootID:         otherRoot.ID,
						AgentType:      "general",
						Model:          "fake-model",
						ReasoningLevel: "",
					},
				)
				return err
			})
			return id, err
		}()
		require.NoError(t, err)
		require.NoError(t, seedChildLink(h.ctx, h.store, subagent.Link{
			ParentID: otherRoot.ID, ChildID: otherChild,
			TaskCallID: "background-" + string(state), Blocking: false, State: state,
		}))
	}
	obligation, err = h.mgr.store.HasBackgroundObligationByRoot(h.ctx, otherRoot.ID)
	require.NoError(t, err)
	assert.False(t, obligation)
	retained, err = h.mgr.retainBudgetForBackground(h.ctx, otherRoot.ID)
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
		service := &budgetServiceProbe{record: &budget.Record{
			State: budget.Armed, Generation: 3,
		}}
		manager := &svc{budgets: service}

		require.NoError(t, manager.releaseArmedBudget(t.Context(), 1, "stopped"))
		assert.Equal(t, 1, service.releaseCalls)
	})

	t.Run("release error is preserved", func(t *testing.T) {
		releaseErr := errors.New("release failed")
		service := &budgetServiceProbe{
			record:     &budget.Record{State: budget.Armed},
			releaseErr: releaseErr,
		}
		manager := &svc{budgets: service}

		err := manager.releaseArmedBudget(t.Context(), 1, "stopped")
		require.ErrorIs(t, err, releaseErr)
		assert.Equal(t, 1, service.releaseCalls)
	})
}
