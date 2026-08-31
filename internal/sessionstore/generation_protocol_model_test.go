package sessionstore

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/transcript"
)

// generationProtocolCommand enumerates the externally meaningful transitions of
// the model-input generation protocol: transcript-entry advancement, output
// emission with insertion-time generation stamping, manager replay, stale
// progress suppression, and the explicit stop lifecycle.
type generationProtocolCommand byte

const (
	genEnqueuePending generationProtocolCommand = iota
	genPromote
	genScheduleTick
	genEmitMessage
	genEmitDirectReply
	genClaim
	genAck
	genStaleProgress
	genStopStart
	genStopCleanupCrash
	genStopComplete
	genRestart

	genCommandCount = int(genRestart) + 1
)

// generationModelRow mirrors one message outbox row the protocol can observe.
type generationModelRow struct {
	generation  int64
	replacing   bool
	releases    bool
	delivered   bool
	sourceKey   string
	inputID     int64
	stopStarted bool
	stopDone    bool
	directReply bool
}

// generationProtocolModel is the protocol oracle. Generation advances only on
// transcript-entry transitions; every emitted row freezes its insertion-time
// generation; stop terminal effects happen at most once.
type generationProtocolModel struct {
	generation int64
	pending    int
	rows       []generationModelRow
	claim      int
	inputSeq   int64
	emitSeq    int64
	stopInput  int64
	stopping   bool
	stopped    bool
	hasBudget  bool
}

func newGenerationModel() *generationProtocolModel {
	return &generationProtocolModel{claim: -1}
}

func (m *generationProtocolModel) head() int {
	for i, row := range m.rows {
		if !row.delivered {
			return i
		}
	}

	return -1
}

func (m *generationProtocolModel) apply(command generationProtocolCommand) {
	switch command {
	case genEnqueuePending:
		if m.stopping {
			// A stopping root no longer accepts model input.
			return
		}

		m.pending++
		m.inputSeq++ // consumes one durable inbox identity
	case genPromote:
		if m.pending == 0 {
			return
		}

		m.pending--
		m.generation++
	case genScheduleTick:
		// Exactly-once delivery: re-delivery never advances or duplicates.
		already := false
		for _, row := range m.rows {
			if row.sourceKey == "schedule:schedule:model:tick:announcement" {
				already = true
			}
		}
		if !already {
			m.generation++
			m.rows = append(m.rows, generationModelRow{
				generation: m.generation, sourceKey: "schedule:schedule:model:tick:announcement",
			})
		}
	case genEmitMessage:
		if m.stopping || m.stopped {
			// Producers are fenced: ordinary output commits fail closed behind
			// the stop fence, so nothing new is emitted.
			return
		}

		m.inputSeq++
		m.emitSeq++
		m.rows = append(m.rows, generationModelRow{
			generation: m.generation,
			replacing:  m.emitSeq%2 == 1,
			sourceKey:  "model:" + strconv.FormatInt(m.emitSeq, 10),
		})
	case genEmitDirectReply:
		if m.stopping || m.stopped {
			return
		}

		m.rows = append(m.rows, generationModelRow{
			generation: m.generation, directReply: true,
		})
	case genClaim:
		if head := m.head(); head >= 0 {
			m.claim = head
		}
	case genAck:
		if m.claim >= 0 {
			row := &m.rows[m.claim]
			row.delivered = true
			if row.stopDone {
				m.stopped = true
			}

			m.claim = -1
		}
	case genStaleProgress:
		// A stale snapshot inserts nothing: modeled as a no-op.
	case genStopStart:
		if m.stopping || m.stopped {
			return
		}

		m.inputSeq++
		m.stopping = true
		m.hasBudget = true
		m.stopInput = m.inputSeq
		m.rows = append(m.rows, generationModelRow{
			generation: m.generation, replacing: true, inputID: m.stopInput,
			sourceKey: "input:" + strconv.FormatInt(m.stopInput, 10) + ":stop:started", stopStarted: true,
		})
	case genStopCleanupCrash:
		// A crash between the fence and the terminal commit changes nothing.
	case genStopComplete:
		if !m.stopping || m.stopped {
			return
		}

		m.stopping = false
		m.hasBudget = false
		m.rows = append(m.rows, generationModelRow{
			generation: m.generation, releases: true, inputID: m.stopInput,
			sourceKey: "input:" + strconv.FormatInt(m.stopInput, 10) + ":stop:completed", stopDone: true,
		})
	case genRestart:
		// A restart changes no protocol state.
	}
}

type generationProduction struct {
	t               *testing.T
	ctx             context.Context
	store           Store
	db              *sql.DB
	root            int64
	inputs          int64
	lastStopInputID int64
	claim           *OutputClaim
}

func newGenerationProduction(t *testing.T) *generationProduction {
	store, db, projectID := newTestStore(t)
	ctx := context.Background()

	root, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "mgr"})
	require.NoError(t, err)
	require.NoError(t, store.BindManager(ctx, "mgr", "cli", map[string]any{"local": true}))

	return &generationProduction{t: t, ctx: ctx, store: store, db: db, root: root.ID}
}

func (p *generationProduction) apply(command generationProtocolCommand) {
	switch command {
	case genEnqueuePending:
		var status string
		require.NoError(p.t, p.db.QueryRowContext(p.ctx,
			`SELECT status FROM sessions WHERE id = ?`, p.root).Scan(&status))
		if status == "stopping" {
			return
		}

		_, err := p.store.EnqueueModelInput(p.ctx, p.root, "queued follow-up")
		require.NoError(p.t, err)
	case genPromote:
		input, err := p.store.PeekPending(p.ctx, p.root)
		if err != nil {
			require.ErrorIs(p.t, err, ErrNoPendingInput)

			return
		}
		_, err = p.store.PromoteInput(p.ctx, input.ID, input.RawContent)
		require.NoError(p.t, err)
	case genScheduleTick:
		_, _, _, err := p.store.InsertToolNotificationPairOnce(
			p.ctx, p.root, "schedule:model:tick", "schedule-model-fingerprint",
			&transcript.Message{
				Role: llmwire.RoleAssistant, ToolCalls: []byte(`[{"id":"s1","name":"schedule"}]`),
			},
			&transcript.Message{
				Role: llmwire.RoleTool, Content: "scheduled event",
				ToolCallID: "s1", ToolName: "schedule",
			},
		)
		require.NoError(p.t, err)
	case genEmitMessage:
		if p.rootStopping() {
			// The fence makes ordinary output commits fail closed.
			return
		}

		p.inputs++
		kind := OutputMessagePersistent
		if p.inputs%2 == 1 {
			kind = OutputMessageReplaceable
		}

		key := "model:" + strconv.FormatInt(p.inputs, 10)
		_, err := p.store.EnqueueOutput(p.ctx, OutputDraft{
			SessionID: p.root, Type: kind, Content: "card " + strconv.FormatInt(p.inputs, 10),
			SourceKey:   key,
			Fingerprint: OutputFingerprint(kind, "card "+strconv.FormatInt(p.inputs, 10), p.root, nil),
		})
		require.NoError(p.t, err)
	case genEmitDirectReply:
		if p.rootStopping() {
			return
		}

		_, _, err := p.store.InsertAssistantMessageWithOutput(p.ctx, p.root, &transcript.Message{
			Role: "assistant", Content: "reply before tool",
			ToolCalls: []byte(`[{"id":"reply-tool","name":"bash"}]`),
		}, OutputMessagePersistent, "reply before tool")
		require.NoError(p.t, err)
	case genClaim:
		if p.claim != nil {
			_, err := p.store.ClaimOutputHead(p.ctx, "mgr")
			require.ErrorIs(p.t, err, ErrNoOutput)

			return
		}

		claim, err := p.store.ClaimOutputHead(p.ctx, "mgr")
		if err == nil {
			p.claim = claim
		} else {
			require.ErrorIs(p.t, err, ErrNoOutput)
		}
	case genAck:
		if p.claim == nil {
			return
		}

		require.NoError(p.t, p.store.AckOutput(
			p.ctx, "mgr", p.claim.Output.ID, p.claim.Output.AttemptID, []string{"1"}, nil,
		))
		p.claim = nil
	case genStaleProgress:
		draft := OutputDraft{
			SessionID: p.root, Type: OutputMessageReplaceable, Content: "stale card",
			SourceKey:   "progress:stale:" + strconv.FormatInt(p.inputs, 10),
			Fingerprint: OutputFingerprint(OutputMessageReplaceable, "stale card", p.root, nil),
		}
		_, err := p.store.EnqueueProgressOutput(p.ctx, draft, p.currentGeneration()-1, SessionStatusActive)
		require.ErrorIs(p.t, err, ErrProgressSuperseded)
	case genStopStart:
		if p.rootStopping() {
			// /stop on a stopping or stopped root never opens a new fence.
			return
		}

		input, err := p.store.EnqueueInput(p.ctx, p.root, InputSourceUser, "/stop")
		require.NoError(p.t, err)
		p.lastStopInputID = input.ID
		_, err = p.store.BeginLifecycleInput(p.ctx, input.ID, "stop", "⏳ Stopping…")
		require.NoError(p.t, err)
		if _, err := p.db.ExecContext(p.ctx, `INSERT INTO session_budgets
			(root_session_id, state, generation, armed_at, baseline_cost_usd, cost_limit_usd)
			VALUES (?, 'armed', 1, datetime('now'), 0, 1)
			ON CONFLICT(root_session_id) DO NOTHING`, p.root); err != nil {
			require.NoError(p.t, err)
		}
	case genStopCleanupCrash:
		// Cleanup is in-flight when the process dies; nothing commits.
	case genStopComplete:
		var inputID sql.NullInt64
		require.NoError(p.t, p.db.QueryRowContext(p.ctx, `SELECT MAX(id) FROM session_inbox
			WHERE session_id = ? AND resolution_reason = 'stop'`, p.root).Scan(&inputID))
		if !inputID.Valid {
			return
		}

		p.lastStopInputID = inputID.Int64
		_, err := p.store.CompleteExplicitStop(p.ctx, p.root, inputID.Int64)
		require.NoError(p.t, err)
	case genRestart:
		p.store = NewStore(p.db)
		p.claim = nil
	}
}

// currentGeneration reads the durable session generation.
func (p *generationProduction) currentGeneration() int64 {
	var generation int64
	require.NoError(p.t, p.db.QueryRowContext(p.ctx,
		`SELECT model_input_generation FROM sessions WHERE id = ?`, p.root).Scan(&generation))

	return generation
}

// rootStopping reports the durable fence state the emit path must respect.
func (p *generationProduction) rootStopping() bool {
	var status string
	require.NoError(p.t, p.db.QueryRowContext(p.ctx,
		`SELECT status FROM sessions WHERE id = ?`, p.root).Scan(&status))

	return status == "stopping" || status == "stopped"
}

func (p *generationProduction) assertMatches(model *generationProtocolModel, step int) {
	actual := p.currentGeneration()
	assert.Equal(p.t, model.generation, actual, "step %d: generation", step)

	var count int
	require.NoError(p.t, p.db.QueryRowContext(p.ctx,
		`SELECT COUNT(*) FROM session_outbox WHERE session_id = ?`, p.root).Scan(&count))
	require.Len(p.t, model.rows, count, "step %d: outbox row count", step)

	rows, err := p.db.QueryContext(p.ctx, `SELECT COALESCE(source_key, ''), attributes,
		releases_input FROM session_outbox WHERE session_id = ? ORDER BY id`, p.root)
	require.NoError(p.t, err)
	defer rows.Close()

	i := 0
	for rows.Next() {
		var sourceKey, attributes string
		var releases bool
		require.NoError(p.t, rows.Scan(&sourceKey, &attributes, &releases))

		want := model.rows[i]
		if want.directReply {
			assert.True(p.t, strings.HasPrefix(sourceKey, "message:"),
				"step %d row %d: direct reply message key", step, i)
			assert.True(p.t, strings.HasSuffix(sourceKey, ":reply"),
				"step %d row %d: direct reply phase", step, i)
		} else if want.stopStarted {
			// The exact inbox id is an implementation detail of identity
			// allocation; the protocol invariant is the started/completed
			// pairing under the same input id.
			assert.True(p.t, strings.HasSuffix(sourceKey, ":stop:started"),
				"step %d row %d: stop start key", step, i)
		} else if want.stopDone {
			assert.True(p.t, strings.HasSuffix(sourceKey, ":stop:completed"),
				"step %d row %d: stop completion key", step, i)
		} else {
			assert.Equal(p.t, want.sourceKey, sourceKey, "step %d row %d: source key", step, i)
		}

		assert.Equal(p.t, want.releases, releases, "step %d row %d: release flag", step, i)
		assert.Contains(p.t, attributes, `"model_input_generation":`, "step %d row %d", step, i)
		assert.Regexp(p.t,
			`"model_input_generation":`+strconv.FormatInt(want.generation, 10)+`[},]`,
			attributes, "step %d row %d: insertion-time generation", step, i)

		i++
	}

	require.NoError(p.t, rows.Err())

	var status string
	require.NoError(p.t, p.db.QueryRowContext(p.ctx,
		`SELECT status FROM sessions WHERE id = ?`, p.root).Scan(&status))
	if model.stopped {
		assert.Equal(p.t, "stopped", status, "step %d: root status", step)
	} else if model.stopping {
		assert.Equal(p.t, "stopping", status, "step %d: root status", step)
	}

	// Exactly-once terminal fact and budget release.
	if model.stopped {
		var completions int
		require.NoError(p.t, p.db.QueryRowContext(p.ctx, `SELECT COUNT(*) FROM session_outbox
			WHERE session_id = ? AND source_key LIKE 'input:%:stop:completed'`, p.root).Scan(&completions))
		assert.Equal(p.t, 1, completions, "step %d: terminal output occurs exactly once", step)

		if model.hasBudget {
			var budgetState string
			require.NoError(p.t, p.db.QueryRowContext(p.ctx, `SELECT state FROM session_budgets
				WHERE root_session_id = ?`, p.root).Scan(&budgetState))
			assert.Equal(p.t, "released", budgetState, "step %d: budget released", step)
		}
	}
}

func runGenerationProtocol(t *testing.T, commands []byte) {
	t.Helper()

	model := newGenerationModel()
	production := newGenerationProduction(t)
	production.assertMatches(model, -1)

	for step, raw := range commands {
		command := generationProtocolCommand(raw % byte(genCommandCount))
		model.apply(command)
		production.apply(command)
		production.assertMatches(model, step)
	}
}

func TestGenerationProtocol_TranscriptEntryAdvancesAndReplayPreserves(t *testing.T) {
	runGenerationProtocol(t, []byte{
		byte(genEmitMessage),     // generation 0 card
		byte(genEnqueuePending),  // pending input must not advance
		byte(genEmitMessage),     // still generation 0
		byte(genPromote),         // promotion advances
		byte(genEmitDirectReply), // direct reply is persistent but non-releasing
		byte(genEmitMessage),     // new generation card
		byte(genClaim),
		byte(genAck),
		byte(genEmitMessage),
		byte(genScheduleTick), // scheduled turn advances
		byte(genScheduleTick), // duplicate delivery: no advance
		byte(genRestart),
		byte(genScheduleTick), // duplicate after restart: no advance
		byte(genEmitMessage),
		byte(genStaleProgress), // stale snapshot inserts nothing
	})
}

func TestGenerationProtocol_ExplicitStopLifecycle(t *testing.T) {
	runGenerationProtocol(t, []byte{
		byte(genStopStart),
		byte(genStopCleanupCrash),
		byte(genStaleProgress), // stale snapshot cannot commit behind the fence
		byte(genRestart),       // crash before terminal commit
		byte(genStopComplete),  // recovery completes the terminal transaction
		byte(genClaim),
		byte(genAck),
		byte(genClaim),
		byte(genAck),
		byte(genClaim),
		byte(genAck),
		byte(genStopComplete), // terminal effects happen exactly once
		byte(genRestart),
		byte(genStopComplete), // still exactly once after restart
	})
}

func FuzzGenerationProtocol(f *testing.F) {
	f.Add([]byte{byte(genEmitMessage), byte(genPromote), byte(genClaim)})
	f.Add([]byte{
		byte(genStopStart), byte(genStopCleanupCrash), byte(genRestart),
		byte(genStopComplete), byte(genAck),
	})
	f.Add([]byte{
		byte(genScheduleTick), byte(genScheduleTick), byte(genEmitMessage),
		byte(genEmitDirectReply), byte(genStaleProgress), byte(genClaim), byte(genAck),
	})

	f.Fuzz(func(t *testing.T, commands []byte) {
		if len(commands) > 24 {
			commands = commands[:24]
		}

		runGenerationProtocol(t, commands)
	})
}
