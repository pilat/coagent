package sessionstore

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/subagent"
)

// harnessCommand is deliberately smaller than the implementation API. These
// are the externally meaningful transitions which may be retried, reordered,
// or replayed after a process restart.
type harnessCommand byte

const (
	harnessFinish harnessCommand = iota
	harnessEnqueueFollowUp
	harnessConsumeInput
	harnessStaleCompletion
	harnessRestart
	harnessFinalizeBeforeCrash
	harnessScheduleTick
	harnessFreshSchedule
	harnessScheduleConflict
	harnessCompact
)

type harnessModel struct {
	activationSeq   int64
	state           string
	delivered       bool
	pendingInputs   int
	parentResults   []string
	scheduleResults []string
	tickDelivered   bool
	freshDelivered  bool
}

func newHarnessModel() *harnessModel {
	return &harnessModel{activationSeq: 1, state: "spawned"}
}

func (m *harnessModel) apply(command harnessCommand) {
	switch command {
	case harnessFinish:
		if m.delivered {
			return
		}

		if m.state == "spawned" || m.state == "running" {
			if m.pendingInputs > 0 {
				return
			}

			m.state = "completed"
		}
		if m.state != "completed" && m.state != "error" {
			return
		}

		m.delivered = true
		m.parentResults = append(m.parentResults, harnessCompletionText(m.activationSeq))
		if m.pendingInputs > 0 {
			m.activationSeq++
			m.state = "running"
			m.delivered = false
		}
	case harnessEnqueueFollowUp:
		m.pendingInputs++
		if m.delivered && (m.state == "completed" || m.state == "error") {
			m.activationSeq++
			m.state = "running"
			m.delivered = false
		}
	case harnessConsumeInput:
		if m.pendingInputs > 0 {
			m.pendingInputs--
		}
	case harnessFinalizeBeforeCrash:
		if m.pendingInputs == 0 && (m.state == "spawned" || m.state == "running") {
			m.state = "completed"
		}
	case harnessScheduleTick:
		if !m.tickDelivered {
			m.tickDelivered = true
			m.scheduleResults = append(m.scheduleResults, "scheduled event")
		}
	case harnessFreshSchedule:
		// The first occurrence replaces the active transcript. Re-delivery of
		// the same identity is a no-op, including after restart.
		if !m.freshDelivered {
			m.freshDelivered = true
			m.parentResults = nil
			m.scheduleResults = nil
		}
	case harnessScheduleConflict:
		// Reusing an identity with a different fingerprint fails closed.
		if !m.tickDelivered {
			m.tickDelivered = true
			m.scheduleResults = append(m.scheduleResults, "scheduled event")
		}
	case harnessCompact:
		// Compaction replaces the whole transcript with one summary row: nothing
		// the parent already saw stays separately observable, and no delivery
		// claim, activation or inbox row moves with it.
		m.parentResults = nil
		m.scheduleResults = nil
	case harnessStaleCompletion, harnessRestart:
		// An old delivery and a process restart cannot change durable protocol state.
	}
}

type harnessProduction struct {
	t      *testing.T
	ctx    context.Context
	db     *sql.DB
	store  Store
	links  subagent.Transactions
	parent int64
	child  int64
	callID string
}

func newHarnessProduction(t *testing.T, migratedDB []byte) *harnessProduction {
	t.Helper()

	ctx := context.Background()
	var store Store
	var db *sql.DB
	var projectID int64
	if migratedDB == nil {
		store, db, projectID = newTestStore(t)
	} else {
		dbPath := filepath.Join(t.TempDir(), "harness.db")
		require.NoError(t, os.WriteFile(dbPath, migratedDB, 0o600))
		var err error
		db, err = migrate.OpenDB(ctx, dbPath)
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		// Fuzzing checks the protocol state machine, not fsync durability. Keep one
		// real SQLite connection but remove disk-flush latency so generated traces
		// explore commands rather than waiting on the temporary filesystem.
		db.SetMaxOpenConns(1)
		_, err = db.ExecContext(ctx, "PRAGMA synchronous=OFF")
		require.NoError(t, err)
		store = NewStore(db)
		result, insertErr := db.ExecContext(
			ctx, `INSERT INTO projects (work_dir, name) VALUES (?, ?)`, t.TempDir(), "harness",
		)
		require.NoError(t, insertErr)
		projectID, err = result.LastInsertId()
		require.NoError(t, err)
	}

	parent, err := store.CreateSession(ctx, projectID, "model", "", nil)
	require.NoError(t, err)
	child, err := store.CreateSubagentSession(
		ctx, projectID, parent.ID, parent.ID, "general", "model", "",
	)
	require.NoError(t, err)
	seedLink(t, db, parent.ID, child, "model-task")

	return &harnessProduction{
		t: t, ctx: ctx, db: db, store: store, links: subagent.NewTransactions(db),
		parent: parent.ID, child: child, callID: "model-task",
	}
}

func (p *harnessProduction) apply(command harnessCommand) {
	p.t.Helper()

	snapshot := p.snapshot()

	switch command {
	case harnessFinish:
		finalized, err := p.links.TryFinalizeActivation(
			p.ctx, p.child, "completed", harnessCompletionText(snapshot.activationSeq), "completed",
		)
		require.NoError(p.t, err)
		if !finalized && (snapshot.state != "completed" && snapshot.state != "error") {
			return
		}

		_, _, err = p.links.DeliverCompletion(
			p.ctx,
			p.parent,
			[]*StoredMessage{{
				Role: llmwire.RoleTool, Content: harnessCompletionText(snapshot.activationSeq),
				ToolCallID: p.callID, ToolName: "subagent_event",
			}},
			p.child,
			snapshot.activationSeq,
		)
		require.NoError(p.t, err)
		rearmed, err := p.links.RearmDeliveredWithPendingInput(p.ctx, p.child)
		require.NoError(p.t, err)
		wantRearmed := snapshot.pendingInputs > 0 &&
			(snapshot.state == "completed" || snapshot.state == "error")
		assert.Equal(p.t, wantRearmed, rearmed)
	case harnessEnqueueFollowUp:
		input, err := p.store.EnqueueInput(
			p.ctx, p.child, InputSourceAgent,
			fmt.Sprintf("follow-up %d.%d", snapshot.activationSeq, snapshot.pendingInputs+1),
		)
		require.NoError(p.t, err)
		rearmed, err := p.links.RearmDeliveredWithPendingInput(p.ctx, p.child)
		require.NoError(p.t, err)
		if snapshot.delivered && (snapshot.state == "completed" || snapshot.state == "error") {
			require.True(p.t, rearmed)
		} else {
			require.False(p.t, rearmed)
		}
		_ = input
	case harnessConsumeInput:
		input, err := p.store.PeekPending(p.ctx, p.child)
		if snapshot.pendingInputs == 0 {
			require.ErrorIs(p.t, err, ErrNoPendingInput)
			return
		}
		require.NoError(p.t, err)
		_, err = p.store.PromoteInput(p.ctx, input.ID, input.RawContent)
		require.NoError(p.t, err)
	case harnessStaleCompletion:
		if snapshot.activationSeq == 1 {
			return
		}

		_, won, err := p.links.DeliverCompletion(
			p.ctx,
			p.parent,
			[]*StoredMessage{{
				Role: llmwire.RoleTool, Content: "stale completion",
				ToolCallID: p.callID, ToolName: "subagent_event",
			}},
			p.child,
			snapshot.activationSeq-1,
		)
		require.NoError(p.t, err)
		assert.False(p.t, won, "a stale activation must never cross the rearm boundary")
	case harnessRestart:
		// The store carries no protocol state in memory. Reconstructing it over the
		// same DB represents a daemon restart at this boundary.
		p.store = NewStore(p.db)
		p.links = subagent.NewTransactions(p.db)
	case harnessFinalizeBeforeCrash:
		finalized, err := p.links.TryFinalizeActivation(
			p.ctx, p.child, "completed", harnessCompletionText(snapshot.activationSeq), "completed",
		)
		require.NoError(p.t, err)
		wantFinalized := snapshot.pendingInputs == 0 &&
			(snapshot.state == "spawned" || snapshot.state == "running")
		assert.Equal(p.t, wantFinalized, finalized)
	case harnessScheduleTick:
		_, _, _, err := p.store.InsertToolNotificationPairOnce(
			p.ctx,
			p.parent,
			"schedule:model:tick",
			"schedule-model-fingerprint",
			&StoredMessage{
				Role:      llmwire.RoleAssistant,
				ToolCalls: []byte(`[{"id":"schedule-model","name":"schedule"}]`),
			},
			&StoredMessage{
				Role: llmwire.RoleTool, Content: "scheduled event",
				ToolCallID: "schedule-model", ToolName: "schedule",
			},
		)
		require.NoError(p.t, err)
	case harnessFreshSchedule:
		_, _, err := p.store.ResetSessionContextOnce(
			p.ctx,
			p.parent,
			"schedule:model:fresh",
			"fresh-model-fingerprint",
			[]*StoredMessage{{Role: llmwire.RoleUser, Content: "fresh scheduled task"}},
		)
		require.NoError(p.t, err)
	case harnessScheduleConflict:
		_, _, _, err := p.store.InsertToolNotificationPairOnce(
			p.ctx,
			p.parent,
			"schedule:model:tick",
			"schedule-model-fingerprint",
			&StoredMessage{
				Role:      llmwire.RoleAssistant,
				ToolCalls: []byte(`[{"id":"schedule-model","name":"schedule"}]`),
			},
			&StoredMessage{
				Role: llmwire.RoleTool, Content: "scheduled event",
				ToolCallID: "schedule-model", ToolName: "schedule",
			},
		)
		require.NoError(p.t, err)

		_, _, _, err = p.store.InsertToolNotificationPairOnce(
			p.ctx,
			p.parent,
			"schedule:model:tick",
			"conflicting-fingerprint",
			&StoredMessage{Role: llmwire.RoleAssistant},
			&StoredMessage{Role: llmwire.RoleTool, Content: "wrong"},
		)
		require.ErrorIs(p.t, err, ErrDeliveryConflict)
	case harnessCompact:
		messages, err := p.store.LoadActiveMessages(p.ctx, p.parent)
		require.NoError(p.t, err)

		compacted := make([]int64, 0, len(messages))
		for _, message := range messages {
			compacted = append(compacted, message.ID)
		}

		_, err = p.store.ReplaceCompactedMessages(p.ctx, p.parent, compacted, []CompactionEntry{
			{Message: &StoredMessage{
				Role: llmwire.RoleUser, Content: "[CONTEXT SUMMARY - previous work condensed]",
			}},
		})
		require.NoError(p.t, err)
	}
}

type harnessSnapshot struct {
	activationSeq   int64
	state           string
	delivered       bool
	pendingInputs   int
	parentResults   []string
	scheduleResults []string
	tickDelivered   bool
	freshDelivered  bool
}

func (p *harnessProduction) snapshot() harnessSnapshot {
	p.t.Helper()

	var snapshot harnessSnapshot
	var deliveredAt sql.NullInt64
	require.NoError(p.t, p.db.QueryRowContext(p.ctx, `
		SELECT activation_seq, state, delivered_at
		FROM subagent_links WHERE child_id = ?`, p.child,
	).Scan(&snapshot.activationSeq, &snapshot.state, &deliveredAt))
	snapshot.delivered = deliveredAt.Valid
	require.NoError(p.t, p.db.QueryRowContext(p.ctx, `
		SELECT COUNT(*) FROM session_inbox
		WHERE session_id = ? AND state = 'pending'`, p.child,
	).Scan(&snapshot.pendingInputs))

	messages, err := p.store.LoadActiveMessages(p.ctx, p.parent)
	require.NoError(p.t, err)
	for _, message := range messages {
		if message.Role == llmwire.RoleTool && message.ToolName == "subagent_event" {
			snapshot.parentResults = append(snapshot.parentResults, message.Content)
		}
		if message.Role == llmwire.RoleTool && message.ToolName == "schedule" {
			snapshot.scheduleResults = append(snapshot.scheduleResults, message.Content)
		}
	}

	require.NoError(p.t, p.db.QueryRowContext(p.ctx, `
		SELECT EXISTS(
			SELECT 1 FROM session_deliveries
			WHERE session_id = ? AND delivery_id = 'schedule:model:tick'
		), EXISTS(
			SELECT 1 FROM session_deliveries
			WHERE session_id = ? AND delivery_id = 'schedule:model:fresh'
		)`, p.parent, p.parent,
	).Scan(&snapshot.tickDelivered, &snapshot.freshDelivered))

	return snapshot
}

func harnessCompletionText(activationSeq int64) string {
	return fmt.Sprintf("activation %d completed", activationSeq)
}

func assertHarnessMatchesModel(t *testing.T, model *harnessModel, production *harnessProduction) {
	t.Helper()

	actual := production.snapshot()
	assert.Equal(t, model.activationSeq, actual.activationSeq, "activation generation")
	assert.Equal(t, model.state, actual.state, "link state")
	assert.Equal(t, model.delivered, actual.delivered, "completion delivery marker")
	assert.Equal(t, model.pendingInputs, actual.pendingInputs, "pending durable inputs")
	assert.Equal(t, model.parentResults, actual.parentResults, "parent-visible completions")
	assert.Equal(t, model.scheduleResults, actual.scheduleResults, "scheduled deliveries")
	assert.Equal(t, model.tickDelivered, actual.tickDelivered, "tick delivery claim")
	assert.Equal(t, model.freshDelivered, actual.freshDelivered, "fresh delivery claim")
}

func runHarnessCommands(t *testing.T, commands, migratedDB []byte) {
	t.Helper()

	model := newHarnessModel()
	production := newHarnessProduction(t, migratedDB)
	assertHarnessMatchesModel(t, model, production)

	for index, raw := range commands {
		command := harnessCommand(raw % byte(harnessCompact+1))
		model.apply(command)
		production.apply(command)
		if !assertHarnessMatchesModelAt(t, index, command, model, production) {
			return
		}
	}
}

func assertHarnessMatchesModelAt(
	t *testing.T,
	index int,
	command harnessCommand,
	model *harnessModel,
	production *harnessProduction,
) bool {
	t.Helper()

	actual := production.snapshot()
	step := fmt.Sprintf("step %d command %d", index, command)
	matched := assert.Equal(t, model.activationSeq, actual.activationSeq, step+": activation")
	matched = assert.Equal(t, model.state, actual.state, step+": state") && matched
	matched = assert.Equal(t, model.delivered, actual.delivered, step+": delivery") && matched
	matched = assert.Equal(t, model.pendingInputs, actual.pendingInputs, step+": inbox") && matched
	matched = assert.Equal(t, model.parentResults, actual.parentResults, step+": results") && matched
	matched = assert.Equal(t, model.scheduleResults, actual.scheduleResults, step+": schedules") && matched
	matched = assert.Equal(t, model.tickDelivered, actual.tickDelivered, step+": tick claim") && matched
	matched = assert.Equal(t, model.freshDelivered, actual.freshDelivered, step+": fresh claim") && matched

	return matched
}

func TestHarnessModel_DeliveryRearmAndRestart(t *testing.T) {
	runHarnessCommands(t, []byte{
		byte(harnessFinish),
		byte(harnessFinish), // at-least-once duplicate
		byte(harnessRestart),
		byte(harnessEnqueueFollowUp),
		byte(harnessStaleCompletion),
		byte(harnessFinish), // pending follow-up prevents terminalization
		byte(harnessConsumeInput),
		byte(harnessFinalizeBeforeCrash),
		byte(harnessRestart),
		byte(harnessFinish), // recovery delivers the terminal undelivered activation
		byte(harnessFinish), // duplicate in the second activation
		byte(harnessEnqueueFollowUp),
		byte(harnessRestart),
		byte(harnessConsumeInput),
		byte(harnessFinish),
	}, nil)
}

func TestHarnessModel_FollowUpAfterTerminalBeforeDeliveryPreservesBothActivations(t *testing.T) {
	runHarnessCommands(t, []byte{
		byte(harnessFinalizeBeforeCrash),
		byte(harnessEnqueueFollowUp),
		byte(harnessRestart),
		byte(harnessFinish), // deliver activation one, then re-arm behind that barrier
		byte(harnessConsumeInput),
		byte(harnessFinish),
	}, nil)
}

func TestHarnessModel_ScheduledDeliveryRetryRestartAndFreshReset(t *testing.T) {
	runHarnessCommands(t, []byte{
		byte(harnessScheduleTick),
		byte(harnessScheduleTick), // producer ack failed: exact duplicate
		byte(harnessRestart),
		byte(harnessScheduleTick), // duplicate after process restart
		byte(harnessScheduleConflict),
		byte(harnessFreshSchedule),
		byte(harnessRestart),
		byte(harnessFreshSchedule), // fresh reset is also exactly once
	}, nil)
}

func TestHarnessModel_CompactionInterleavedWithDeliveryAndRestart(t *testing.T) {
	runHarnessCommands(t, []byte{
		byte(harnessScheduleTick),
		byte(harnessFinish),
		byte(harnessCompact), // the summary swallows both, claims stay claimed
		byte(harnessScheduleTick),
		byte(harnessFinish), // at-least-once duplicate after the swap
		byte(harnessEnqueueFollowUp),
		byte(harnessConsumeInput),
		byte(harnessRestart),
		byte(harnessStaleCompletion),
		byte(harnessFinish), // the new activation still reaches the parent
		byte(harnessCompact),
		byte(harnessStaleCompletion),
	}, nil)
}

func TestHarnessModel_PromotedRootRemainsRunnableAcrossRestart(t *testing.T) {
	modelRunnable := false
	production := newHarnessProduction(t, nil)

	requireRootRunnable := func() {
		t.Helper()

		recoverable, err := production.store.ListSessionsWithRecoverableInput(production.ctx)
		require.NoError(t, err)
		assert.Equal(t, modelRunnable, containsSessionID(recoverable, production.parent))
	}

	input, err := production.store.EnqueueInput(
		production.ctx, production.parent, InputSourceUser, "root input before crash",
	)
	require.NoError(t, err)
	_, err = production.store.PromoteInput(production.ctx, input.ID, "root input before crash")
	require.NoError(t, err)
	modelRunnable = true
	requireRootRunnable()

	production.store = NewStore(production.db)
	requireRootRunnable()

	_, err = production.store.InsertMessage(production.ctx, production.parent, &StoredMessage{
		Role: llmwire.RoleAssistant, Content: "answered",
	})
	require.NoError(t, err)
	requireRootRunnable() // restart settles the persisted final without republishing it

	require.NoError(t, production.store.UpdateSessionStatus(
		production.ctx, production.parent, SessionStatusCompleted,
	))
	modelRunnable = false
	requireRootRunnable()

	production.store = NewStore(production.db)
	requireRootRunnable()
}

func containsSessionID(ids []int64, target int64) bool {
	return slices.Contains(ids, target)
}

func FuzzHarnessProtocol(f *testing.F) {
	templatePath := filepath.Join(f.TempDir(), "harness-template.db")
	db, err := migrate.OpenDB(f.Context(), templatePath)
	require.NoError(f, err)
	require.NoError(f, migrate.Run(f.Context(), db, ""))
	require.NoError(f, db.Close())
	migratedDB, err := os.ReadFile(templatePath)
	require.NoError(f, err)

	f.Add([]byte{0})
	f.Add([]byte{0, 0, 1, 3, 0, 2, 0})
	f.Add([]byte{0, 4, 1, 4, 3, 2, 5, 0, 0})

	f.Fuzz(func(t *testing.T, commands []byte) {
		if len(commands) > 32 {
			commands = commands[:32]
		}

		runHarnessCommands(t, commands, migratedDB)
	})
}
