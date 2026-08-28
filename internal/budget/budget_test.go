package budget

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

// budgetFixture builds a real store plus one root with a pending /budget grant,
// mirroring how the daemon hands the tool its authority.
type budgetFixture struct {
	t      *testing.T
	db     *sql.DB
	store  sessionstore.Store
	svc    Service
	rootID int64
	input  *sessionstore.InboxInput
	grants int
}

func newBudgetFixture(ctx context.Context, t *testing.T) *budgetFixture {
	t.Helper()

	db, err := migrate.OpenDB(ctx, filepath.Join(t.TempDir(), "budget.db"))
	require.NoError(t, err)
	require.NoError(t, migrate.Run(ctx, db, filepath.Join(t.TempDir(), "unused.db")))
	t.Cleanup(func() { _ = db.Close() })

	res, err := db.ExecContext(ctx, `INSERT INTO projects (work_dir, name) VALUES (?, ?)`,
		t.TempDir(), "budget-test")
	require.NoError(t, err)
	projectID, err := res.LastInsertId()
	require.NoError(t, err)

	store := sessionstore.NewStore(db)
	root, err := store.CreateSession(ctx, projectID, "priced", "", map[string]any{"manager_id": "cli"})
	require.NoError(t, err)
	input := fGrant(ctx, t, store, root.ID)

	return &budgetFixture{t: t, db: db, store: store, svc: New(store), rootID: root.ID, input: input}
}

// fGrant enqueues one /budget user input and promotes it with a pending grant.
func fGrant(ctx context.Context, t *testing.T, store sessionstore.Store, rootID int64) *sessionstore.InboxInput {
	t.Helper()

	input, err := store.EnqueueInput(ctx, rootID, sessionstore.InputSourceUser, "/budget five dollars")
	require.NoError(t, err)
	_, _, err = store.PromoteInputWithActivation(ctx, input.ID, "/budget five dollars\n\nactivate",
		sessionstore.ActivationDraft{ToolID: ToolID, Command: "/budget"})
	require.NoError(t, err)

	return input
}

// seedCost writes one message row carrying cost, so a later arm captures a
// non-zero baseline.
func (f *budgetFixture) seedCost(ctx context.Context, cost float64) {
	f.t.Helper()

	_, err := f.db.ExecContext(ctx,
		`INSERT INTO messages (session_id, role, content, cost_usd) VALUES (?, 'user', 'seed', ?)`,
		f.rootID, cost)
	require.NoError(f.t, err)
}

func (f *budgetFixture) checkpointContent(ctx context.Context, generation int64) string {
	f.t.Helper()

	var content string
	err := f.db.QueryRowContext(ctx,
		`SELECT content FROM session_outbox WHERE source_key = ?`,
		fmt.Sprintf("budget:%d:checkpoint", generation)).Scan(&content)
	require.NoError(f.t, err)

	return content
}

func (f *budgetFixture) grant() Grant {
	return Grant{
		RootID: f.rootID, InputID: f.input.ID, ToolID: ToolID,
		Command: "/budget", ToolCallID: "call-budget-1",
	}
}

// newGrant creates a fresh user turn with its own pending grant.
func (f *budgetFixture) newGrant(ctx context.Context) Grant {
	f.grants++

	input, err := f.store.EnqueueInput(ctx, f.rootID, sessionstore.InputSourceUser, "/budget more")
	require.NoError(f.t, err)
	_, _, err = f.store.PromoteInputWithActivation(ctx, input.ID, "/budget more\n\nactivate",
		sessionstore.ActivationDraft{ToolID: ToolID, Command: "/budget"})
	require.NoError(f.t, err)

	return Grant{
		RootID: f.rootID, InputID: input.ID, ToolID: ToolID,
		Command: "/budget", ToolCallID: "call-budget-" + strconv.Itoa(f.grants+1),
	}
}

// TestBudgetServiceSetClearAndRearm covers the one-shot lifecycle: arm replaces
// the whole limit and bumps the generation; clear releases; re-arm starts fresh.
func TestBudgetServiceSetClearAndRearm(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	f := newBudgetFixture(ctx, t)

	cost := 5.0
	armed, receipt, err := f.svc.Set(ctx, f.grant(), &cost, nil)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.BudgetArmed, armed.State)
	assert.Equal(t, int64(1), armed.Generation)
	assert.Equal(t, "Budget armed: $5.000000 additional persisted cost", receipt)

	// A duplicate arm with the same consumed grant is the crash-replay
	// contract: the same generation and receipt, no second mutation.
	replayed, replayReceipt, err := f.svc.Set(ctx, f.grant(), &cost, nil)
	require.NoError(t, err)
	assert.Equal(t, armed.Generation, replayed.Generation)
	assert.Equal(t, receipt, replayReceipt)

	rearmed, receipt2, err := f.svc.Set(ctx, f.newGrant(ctx), nil, ptrDuration(2*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(2), rearmed.Generation, "a re-arm bumps the generation")
	assert.Equal(t, sessionstore.BudgetArmed, rearmed.State)
	assert.Nil(t, rearmed.CostLimitUSD, "a re-arm replaces the whole limit")
	assert.Equal(t, "Budget armed: 2h0m0s wall time", receipt2)

	cleared, clearReceipt, err := f.svc.Clear(ctx, f.newGrant(ctx))
	require.NoError(t, err)
	assert.Equal(t, sessionstore.BudgetReleased, cleared.State)
	assert.Equal(t, "Budget cleared", clearReceipt)

	// Clearing with no budget present still answers the turn.
	_, _, err = f.svc.Clear(ctx, f.newGrant(ctx))
	require.NoError(t, err)
}

func TestBudgetServiceAdmit(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	f := newBudgetFixture(ctx, t)
	now := time.Now().UTC()

	require.NoError(t, f.svc.Admit(ctx, f.rootID, now), "no budget admits")

	cost := 5.0
	armed, _, err := f.svc.Set(ctx, f.grant(), &cost, nil)
	require.NoError(t, err)
	require.NoError(t, f.svc.Admit(ctx, f.rootID, time.Now().UTC()), "armed admits at a later clock")

	require.Error(t, f.svc.Admit(ctx, f.rootID, armed.ArmedAt.Add(-time.Minute)),
		"a clock predating the baseline must not admit new work")

	_, _, err = budgetStore(f).FireBudget(ctx, f.rootID, armed.Generation, "cost", 6, "fired")
	require.NoError(t, err)
	require.ErrorContains(t, f.svc.Admit(ctx, f.rootID, now), "budget checkpoint fired")

	// A released budget admits regardless of the clock: the early return comes
	// before the baseline check.
	_, err = budgetStore(f).ReleaseBudget(ctx, f.rootID, armed.Generation, "resumed")
	require.NoError(t, err)
	require.NoError(t, f.svc.Admit(ctx, f.rootID, armed.ArmedAt.Add(-time.Minute)))
}

func TestBudgetServiceObserveFirePrecedence(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	armWithDuration := func(t *testing.T, cost *float64, duration time.Duration) *budgetFixture {
		t.Helper()

		f := newBudgetFixture(ctx, t)
		_, _, err := f.svc.Set(ctx, f.grant(), cost, &duration)
		require.NoError(t, err)

		return f
	}

	t.Run("below both limits does not fire", func(t *testing.T) {
		t.Parallel()

		f := armWithDuration(t, ptrFloat(5), time.Hour)
		f.seedCost(ctx, 1)
		_, fired, err := f.svc.Observe(ctx, f.rootID, 0, time.Now().UTC(), "")
		require.NoError(t, err)
		assert.False(t, fired)
	})

	t.Run("cost crossing fires with the checkpoint receipt", func(t *testing.T) {
		t.Parallel()

		f := newBudgetFixture(ctx, t)
		f.seedCost(ctx, 1.25)
		cost := 5.0
		_, _, err := f.svc.Set(ctx, f.grant(), &cost, ptrDuration(365*24*time.Hour))
		require.NoError(t, err)
		// Delta counts only cost accumulated after the arm baseline: the arm saw
		// 1.25, the observation's own transaction sums 6.25.
		f.seedCost(ctx, 5)
		record, fired, err := f.svc.Observe(
			ctx, f.rootID, 0, time.Now().UTC(), "Working on it.")
		require.NoError(t, err)
		require.Equal(t, sessionstore.BudgetFired, record.State)
		assert.Equal(t, "cost", record.FiredReason)
		assert.True(t, fired, "a crossing Observe reports the fire to its caller")
		require.NotNil(t, record.ObservedCostUSD)
		assert.InDelta(t, 5.0, *record.ObservedCostUSD, 1e-9)
		assert.Contains(t, f.checkpointContent(ctx, record.Generation),
			"Working on it.\n\nBudget checkpoint reached (cost). Persisted cost: $5.000000.")

		// A duplicate observer sees the fired generation and reports it as such.
		f.seedCost(ctx, 3)
		_, fired, err = f.svc.Observe(ctx, f.rootID, 0, time.Now().UTC(), "")
		require.NoError(t, err)
		assert.True(t, fired)
	})

	t.Run("delta exactly at the limit fires", func(t *testing.T) {
		t.Parallel()

		f := newBudgetFixture(ctx, t)
		f.seedCost(ctx, 0.25)
		cost := 5.0
		_, _, err := f.svc.Set(ctx, f.grant(), &cost, ptrDuration(365*24*time.Hour))
		require.NoError(t, err)
		f.seedCost(ctx, 5)
		record, _, err := f.svc.Observe(
			ctx, f.rootID, 0, time.Now().UTC(), "")
		require.NoError(t, err)
		assert.Equal(t, sessionstore.BudgetFired, record.State, "delta == limit must fire")
		assert.NotContains(t, f.checkpointContent(ctx, record.Generation), "prefix must be absent for empty text\n\n")
	})

	t.Run("duration deadline crossed at observation wins over cost", func(t *testing.T) {
		t.Parallel()

		f := armWithDuration(t, ptrFloat(0.01), time.Hour)
		f.seedCost(ctx, 5)
		armedAt := time.Now().UTC()
		record, fired, err := f.svc.Observe(
			ctx, f.rootID, 0, armedAt.Add(time.Hour), "",
		)
		require.NoError(t, err)
		require.Equal(t, sessionstore.BudgetFired, record.State)
		assert.Equal(t, "duration", record.FiredReason)
		assert.True(t, fired)
	})

	t.Run("cost wins while the duration deadline is still ahead", func(t *testing.T) {
		t.Parallel()

		f := armWithDuration(t, ptrFloat(0.01), time.Hour)
		f.seedCost(ctx, 5)
		armedAt := time.Now().UTC()
		record, fired, err := f.svc.Observe(
			ctx, f.rootID, 0, armedAt.Add(30*time.Minute), "",
		)
		require.NoError(t, err)
		require.Equal(t, sessionstore.BudgetFired, record.State)
		assert.Equal(t, "cost", record.FiredReason)
		assert.True(t, fired)
	})

	t.Run("released budget stops observing", func(t *testing.T) {
		t.Parallel()

		f := armWithDuration(t, ptrFloat(5), time.Hour)
		_, err := budgetStore(f).ReleaseBudget(ctx, f.rootID, 1, "resumed")
		require.NoError(t, err)

		f.seedCost(ctx, 100)
		_, fired, err := f.svc.Observe(ctx, f.rootID, 0, time.Now().UTC(), "")
		require.NoError(t, err)
		assert.False(t, fired)
	})
}

// TestBudgetToolAuthorizationAndReceipts pins the activation contract: get is
// free, set/clear need the exact current grant, and receipts are direct output.
//
//nolint:contextcheck // the fixture's store contexts are intentionally per-fixture
func TestBudgetToolAuthorizationAndReceipts(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	f := newBudgetFixture(ctx, t)
	pricedTool := NewTool(f.svc, f.rootID, true)
	toolFor := func(fx *budgetFixture) tool.Tool { return NewTool(fx.svc, fx.rootID, true) }

	authoredCtx := func(grant Grant) context.Context {
		return tool.WithActivationGrant(tool.WithCallID(ctx, grant.ToolCallID), tool.ActivationGrant{
			SessionID: grant.RootID, InputID: grant.InputID, ToolID: grant.ToolID,
			Command: grant.Command,
		})
	}

	t.Run("get needs no grant and reports absence", func(t *testing.T) {
		result, err := pricedTool.Execute(ctx, mustJSON(t, map[string]any{"action": "get"}))
		require.NoError(t, err)
		assert.Equal(t, "No budget is configured.", result.Output)
	})

	t.Run("set without a grant is refused and leaves it pending", func(t *testing.T) {
		cost := 5.0
		_, err := pricedTool.Execute(ctx, mustJSON(t, map[string]any{"action": "set", "cost_usd": cost}))
		require.EqualError(t, err, noActivationMessage)

		// The grant was not consumed: an authorized retry still arms.
		result, err := pricedTool.Execute(authoredCtx(f.grant()), mustJSON(t, map[string]any{
			"action": "set", "cost_usd": cost,
		}))
		require.NoError(t, err)
		assert.Equal(t, []string{"Budget armed: $5.000000 additional persisted cost"}, result.DirectMessages)
	})

	t.Run("grant identity is checked field by field", func(t *testing.T) {
		f2 := newBudgetFixture(ctx, t)
		f2Tool := toolFor(f2)

		refused := func(t *testing.T, grant Grant) {
			t.Helper()

			_, err := f2Tool.Execute(authoredCtx(grant), mustJSON(t, map[string]any{
				"action": "set", "cost_usd": 1.0,
			}))
			require.EqualError(t, err, noActivationMessage)
		}

		base := f2.grant()

		wrongTool := base
		wrongTool.ToolID = "other_tool"
		refused(t, wrongTool)

		wrongCommand := base
		wrongCommand.Command = "/budgetx"
		refused(t, wrongCommand)

		wrongSessionCtx := tool.WithActivationGrant(tool.WithCallID(ctx, base.ToolCallID),
			tool.ActivationGrant{
				SessionID: f2.rootID + 1, InputID: base.InputID, ToolID: ToolID, Command: "/budget",
			})
		_, err := f2Tool.Execute(wrongSessionCtx, mustJSON(t, map[string]any{
			"action": "set", "cost_usd": 1.0,
		}))
		require.EqualError(t, err, noActivationMessage)
	})

	t.Run("get rejects limit fields", func(t *testing.T) {
		_, err := pricedTool.Execute(ctx, mustJSON(t, map[string]any{"action": "get", "cost_usd": 1.0}))
		assert.EqualError(t, err, "get does not accept limit fields")
	})

	t.Run("unpriced model refuses cost but allows duration", func(t *testing.T) {
		f2 := newBudgetFixture(ctx, t)
		unpriced := NewTool(f2.svc, f2.rootID, false)

		_, err := unpriced.Execute(authoredCtx(f2.grant()), mustJSON(t, map[string]any{
			"action": "set", "cost_usd": 5.0,
		}))
		require.EqualError(t, err, "cost budget unavailable: the current model has no catalog pricing")

		// The pricing refusal left the grant pending: a corrected call uses it.
		result, err := unpriced.Execute(authoredCtx(f2.grant()), mustJSON(t, map[string]any{
			"action": "set", "duration": "2h",
		}))
		require.NoError(t, err)
		assert.NotEmpty(t, result.DirectMessages)
	})

	t.Run("invalid arguments leave the grant pending", func(t *testing.T) {
		f2 := newBudgetFixture(ctx, t)
		f2Tool := toolFor(f2)

		_, err := f2Tool.Execute(authoredCtx(f2.grant()), mustJSON(t, map[string]any{
			"action": "set", "cost_usd": -1.0,
		}))
		require.Error(t, err)
		assert.NotContains(t, err.Error(), noActivationMessage)

		_, err = f2Tool.Execute(authoredCtx(f2.grant()), mustJSON(t, map[string]any{
			"action": "set", "duration": "2026-08-27T10:00:00Z",
		}))
		require.Error(t, err)

		// Still pending: the corrected call succeeds with the same grant.
		result, err := f2Tool.Execute(authoredCtx(f2.grant()), mustJSON(t, map[string]any{
			"action": "set", "cost_usd": 3.0,
		}))
		require.NoError(t, err)
		assert.NotEmpty(t, result.DirectMessages)
	})

	t.Run("clear consumes the grant and emits its receipt", func(t *testing.T) {
		f2 := newBudgetFixture(ctx, t)
		cost := 2.0
		_, _, err := f2.svc.Set(ctx, f2.grant(), &cost, nil)
		require.NoError(t, err)

		f2Tool := toolFor(f2)
		grant2 := f2.newGrant(ctx)
		result, err := f2Tool.Execute(authoredCtx(grant2), mustJSON(t, map[string]any{
			"action": "clear",
		}))
		require.NoError(t, err)
		assert.Equal(t, []string{"Budget cleared"}, result.DirectMessages)

		// A second matching call in a later turn (new tool_call id) is refused
		// with the exact contract text.
		_, err = f2Tool.Execute(tool.WithActivationGrant(tool.WithCallID(ctx, "call-later"),
			tool.ActivationGrant{
				SessionID: f2.rootID, InputID: grant2.InputID, ToolID: ToolID,
				Command: "/budget", ToolCallID: "call-budget-2",
			}), mustJSON(t, map[string]any{"action": "clear"}))
		require.EqualError(t, err, noActivationMessage)
	})

	t.Run("clear rejects limit fields", func(t *testing.T) {
		f2 := newBudgetFixture(ctx, t)
		_, err := toolFor(f2).Execute(authoredCtx(f2.grant()), mustJSON(t, map[string]any{
			"action": "clear", "duration": "2h",
		}))
		assert.EqualError(t, err, "clear does not accept limit fields")
	})

	t.Run("invalid action is a model-facing error", func(t *testing.T) {
		_, err := pricedTool.Execute(ctx, mustJSON(t, map[string]any{"action": "arm"}))
		assert.EqualError(t, err, "action must be get, set, or clear")
	})

	t.Run("malformed arguments are a model-facing error", func(t *testing.T) {
		_, err := pricedTool.Execute(ctx, json.RawMessage(`{not json`))
		assert.ErrorContains(t, err, "invalid parameters")
	})
}

func budgetStore(f *budgetFixture) sessionstore.BudgetStore {
	return f.store.(sessionstore.BudgetStore)
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()

	encoded, err := json.Marshal(value)
	require.NoError(t, err)

	return encoded
}
