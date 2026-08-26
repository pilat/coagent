package sessionstore

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/migrate"
)

type outputProtocolCommand byte

const (
	outputEmitA outputProtocolCommand = iota
	outputEmitB
	outputClaim
	outputAck
	outputStaleAck
	outputRetry
	outputWake
	outputRestart
	outputBlock

	outputCommandCount = int(outputBlock) + 1
)

// The model tracks two manager queues sharing one database: a command byte
// selects both the operation and which manager's queue it lands on, so the
// fuzz can interleave the queues and expose cross-manager interference.
type outputProtocolRow struct {
	manager  int
	state    OutputState
	attempts int64
	due      bool
}

type outputProtocolModel struct {
	rows    []outputProtocolRow
	claimed [2]int
}

func newOutputProtocolModel() *outputProtocolModel {
	return &outputProtocolModel{claimed: [2]int{-1, -1}}
}

// head is the manager's lowest unresolved row, the only one a claim may touch.
func (m *outputProtocolModel) head(manager int) int {
	for i, row := range m.rows {
		if row.manager == manager && row.state != OutputStateDelivered {
			return i
		}
	}

	return -1
}

func (m *outputProtocolModel) apply(command outputProtocolCommand, manager int) {
	switch command {
	case outputEmitA, outputEmitB:
		m.rows = append(m.rows, outputProtocolRow{
			manager: manager, state: OutputStatePending,
		})
	case outputClaim:
		if m.claimed[manager] >= 0 {
			return
		}

		head := m.head(manager)
		if head < 0 {
			return
		}

		row := &m.rows[head]
		if row.state == OutputStatePending || (row.state == OutputStateRetryWait && row.due) {
			row.state = OutputStateDelivering
			row.attempts++
			row.due = false
			m.claimed[manager] = head
		}
	case outputAck:
		if m.claimed[manager] >= 0 {
			m.rows[m.claimed[manager]].state = OutputStateDelivered
			m.claimed[manager] = -1
		}
	case outputRetry:
		if m.claimed[manager] >= 0 {
			i := m.claimed[manager]
			m.rows[i].state = OutputStateRetryWait
			m.rows[i].due = false
			m.claimed[manager] = -1
		}
	case outputWake:
		// A wake makes only the manager's own retry_wait head immediately due.
		if head := m.head(manager); head >= 0 && m.rows[head].state == OutputStateRetryWait {
			m.rows[head].due = true
		}
	case outputRestart:
		// Boot recovery moves every interrupted attempt to an immediately
		// due retry, across every manager.
		for i := range m.rows {
			if m.rows[i].state == OutputStateDelivering {
				m.rows[i].state = OutputStateRetryWait
				m.rows[i].due = true
			}
		}

		m.claimed = [2]int{-1, -1}
	case outputBlock:
		if m.claimed[manager] >= 0 {
			m.rows[m.claimed[manager]].state = OutputStateBlocked
			m.claimed[manager] = -1
		}
	case outputStaleAck:
		// A stale attempt cannot change the claimed row.
	}
}

type outputProtocolProduction struct {
	t       *testing.T
	ctx     context.Context
	store   Store
	db      *sql.DB
	session [2]int64
	claim   [2]*OutputClaim
	staleID string
	emitted [2]int
}

var outputModelManagers = [2]string{"model-manager-0", "model-manager-1"}

func newOutputProtocolProduction(t *testing.T, migratedDB []byte) *outputProtocolProduction {
	t.Helper()

	store, db, projectID := newTemplateTestStore(t, migratedDB)
	var production outputProtocolProduction

	for i, manager := range outputModelManagers {
		record, err := store.CreateSession(
			context.Background(),
			projectID,
			"model",
			"",
			map[string]any{"manager_id": manager},
		)
		require.NoError(t, err)
		require.NoError(t, store.BindManager(context.Background(), manager, "cli", map[string]any{"local": true}))
		production.session[i] = record.ID
	}

	production.t, production.ctx, production.store, production.db = t, context.Background(), store, db

	return &production
}

func (p *outputProtocolProduction) apply(raw byte) {
	p.t.Helper()

	command := outputProtocolCommand(raw % byte(outputCommandCount))
	manager := int(raw / byte(outputCommandCount) % 2)
	ctx, store := p.ctx, p.store

	switch command {
	case outputEmitA, outputEmitB:
		name := "a"
		if command == outputEmitB {
			name = "b"
		}

		p.emitted[manager]++
		key := fmt.Sprintf("model:%d:%s:%d", manager, name, p.emitted[manager])
		_, err := store.EnqueueOutput(ctx, OutputDraft{
			SessionID: p.session[manager], Type: OutputMessagePersistent, Content: name,
			SourceKey: key,
			Fingerprint: OutputFingerprint(
				OutputMessagePersistent,
				name,
				p.session[manager],
				nil,
			),
		})
		require.NoError(p.t, err)
	case outputClaim:
		claim, err := store.ClaimOutputHead(ctx, outputModelManagers[manager])
		if p.claim[manager] == nil {
			if err == nil {
				p.claim[manager] = claim
			}

			return
		}
		require.ErrorIs(p.t, err, ErrNoOutput)
	case outputAck:
		if p.claim[manager] == nil {
			return
		}

		claim := p.claim[manager]
		require.NoError(
			p.t,
			store.AckOutput(
				ctx,
				outputModelManagers[manager],
				claim.Output.ID,
				claim.Output.AttemptID,
				[]string{},
				nil,
			),
		)
		p.claim[manager] = nil
	case outputStaleAck:
		if p.staleID == "" || p.claim[manager] == nil {
			return
		}

		claim := p.claim[manager]
		err := store.AckOutput(ctx, outputModelManagers[manager], claim.Output.ID, p.staleID, []string{}, nil)
		require.ErrorIs(p.t, err, ErrOutputAttempt)
	case outputRetry:
		if p.claim[manager] == nil {
			return
		}

		claim := p.claim[manager]
		p.staleID = claim.Output.AttemptID
		require.NoError(
			p.t,
			store.RetryOutput(
				ctx,
				outputModelManagers[manager],
				claim.Output.ID,
				claim.Output.AttemptID,
				"temporary",
				time.Now().UTC().Add(time.Hour),
			),
		)
		p.claim[manager] = nil
	case outputWake:
		_, err := store.WakeOutputHead(ctx, outputModelManagers[manager])
		require.NoError(p.t, err)
	case outputRestart:
		_, err := store.RecoverInterruptedOutputs(ctx)
		require.NoError(p.t, err)
		p.claim = [2]*OutputClaim{nil, nil}
	case outputBlock:
		if p.claim[manager] == nil {
			return
		}

		claim := p.claim[manager]
		require.NoError(
			p.t,
			store.BlockOutput(
				ctx,
				outputModelManagers[manager],
				claim.Output.ID,
				claim.Output.AttemptID,
				"invalid target",
			),
		)
		p.claim[manager] = nil
	}
}

func (p *outputProtocolProduction) states() ([]OutputState, []int64) {
	rows, err := p.db.QueryContext(p.ctx, `SELECT state, attempt_seq FROM session_outbox ORDER BY id`)
	require.NoError(p.t, err)
	defer rows.Close()
	states := make([]OutputState, 0)
	attempts := make([]int64, 0)
	for rows.Next() {
		var state OutputState
		var attempt int64
		require.NoError(p.t, rows.Scan(&state, &attempt))
		states, attempts = append(states, state), append(attempts, attempt)
	}
	require.NoError(p.t, rows.Err())

	return states, attempts
}

func runOutputProtocolModel(t *testing.T, commands []byte) {
	t.Helper()

	runOutputProtocolModelOnDB(t, commands, nil)
}

// runOutputProtocolModelOnDB optionally starts from a pre-migrated template so
// fuzzing pays for migrations once, not per exec.
func runOutputProtocolModelOnDB(t *testing.T, commands, migratedDB []byte) {
	t.Helper()
	model := newOutputProtocolModel()
	production := newOutputProtocolProduction(t, migratedDB)
	for i, raw := range commands {
		model.apply(outputProtocolCommand(raw%byte(outputCommandCount)), int(raw/byte(outputCommandCount)%2))
		production.apply(raw)
		states, attempts := production.states()
		assert.Equal(t, modelStates(model), states, "step %d states", i)
		assert.Equal(t, modelAttempts(model), attempts, "step %d attempts", i)
	}
}

func modelStates(model *outputProtocolModel) []OutputState {
	states := make([]OutputState, len(model.rows))
	for i, row := range model.rows {
		states[i] = row.state
	}

	return states
}

func modelAttempts(model *outputProtocolModel) []int64 {
	attempts := make([]int64, len(model.rows))
	for i, row := range model.rows {
		attempts[i] = row.attempts
	}

	return attempts
}

// newTemplateTestStore is newTestStore over an optional pre-migrated template
// file, with fsync latency removed: fuzzing explores the protocol state
// machine, not disk durability.
func newTemplateTestStore(t *testing.T, migratedDB []byte) (Store, *sql.DB, int64) {
	t.Helper()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")

	if migratedDB != nil {
		require.NoError(t, os.WriteFile(dbPath, migratedDB, 0o600))
	} else {
		db, err := migrate.OpenDB(ctx, dbPath)
		require.NoError(t, err)
		require.NoError(t, migrate.Run(ctx, db, dbPath))
		require.NoError(t, db.Close())
	}

	db, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	_, err = db.ExecContext(ctx, "PRAGMA synchronous=OFF")
	require.NoError(t, err)

	res, err := db.ExecContext(
		ctx,
		`INSERT INTO projects (work_dir, name) VALUES (?, ?)`,
		t.TempDir(), "test",
	)
	require.NoError(t, err)
	projectID, err := res.LastInsertId()
	require.NoError(t, err)

	return NewStore(db), db, projectID
}

func TestHarnessModel_ManagerOutputFIFOAndAttemptCAS(t *testing.T) {
	runOutputProtocolModel(t, []byte{
		byte(outputEmitA), byte(outputClaim), byte(outputRetry),
		byte(outputWake), byte(outputClaim), byte(outputStaleAck), byte(outputAck),
		byte(outputClaim), byte(outputRestart), byte(outputClaim), byte(outputAck),
	})
}

// A blocked or not-yet-due head hides every later row of its own manager while
// the other manager's queue keeps draining independently.
func TestHarnessModel_ManagerOutputHeadOfLineBlocking(t *testing.T) {
	runOutputProtocolModel(t, []byte{
		byte(outputEmitA), byte(outputEmitB) | byte(outputCommandCount), byte(outputClaim), byte(outputBlock),
		byte(outputClaim), byte(outputClaim) | byte(outputCommandCount), byte(outputAck) | byte(outputCommandCount),
		byte(outputWake),
	})
	runOutputProtocolModel(t, []byte{
		byte(outputEmitA), byte(outputClaim), byte(outputRetry), byte(outputClaim),
		byte(outputWake), byte(outputClaim), byte(outputAck),
	})
}

// Two interleaved manager queues never block or reorder one another: while
// manager 0's head sits blocked, manager 1 still drains its own rows in order.
func TestHarnessModel_ManagerQueuesAreIndependent(t *testing.T) {
	runOutputProtocolModel(t, []byte{
		byte(outputEmitA), byte(outputEmitA), // two rows for manager 0
		byte(outputEmitB) | byte(outputCommandCount),
		byte(outputClaim), byte(outputBlock), // manager 0 blocks its head
		byte(outputClaim) | byte(outputCommandCount), byte(outputAck) | byte(outputCommandCount),
	})
	runOutputProtocolModel(t, []byte{
		byte(outputEmitA), byte(outputEmitB) | byte(outputCommandCount),
		byte(outputClaim), byte(outputRetry), // manager 0 waits into the future
		byte(outputClaim) | byte(outputCommandCount), byte(outputAck) | byte(outputCommandCount),
		byte(outputClaim) | byte(outputCommandCount), byte(outputAck) | byte(outputCommandCount),
		byte(outputClaim), byte(outputAck),
	})
}

// The output protocol gets its own fuzz target: sharing the harness bytes
// doubled every exec's cost, and the shared corpus starved both models.
func FuzzManagerOutputProtocol(f *testing.F) {
	templatePath := filepath.Join(f.TempDir(), "output-template.db")
	db, err := migrate.OpenDB(f.Context(), templatePath)
	require.NoError(f, err)
	require.NoError(f, migrate.Run(f.Context(), db, ""))
	require.NoError(f, db.Close())
	migratedDB, err := os.ReadFile(templatePath)
	require.NoError(f, err)

	f.Add([]byte{byte(outputEmitA), byte(outputEmitB), byte(outputClaim), byte(outputAck)})
	f.Add([]byte{
		byte(outputEmitA),
		byte(outputClaim),
		byte(outputRetry),
		byte(outputWake),
		byte(outputClaim),
		byte(outputStaleAck),
	})
	f.Add([]byte{
		byte(outputEmitA), byte(outputEmitB) | byte(outputCommandCount),
		byte(outputClaim) | byte(2*outputCommandCount), byte(outputClaim),
		byte(outputAck) | byte(3*outputCommandCount), byte(outputRestart),
		byte(outputClaim), byte(outputStaleAck) | byte(outputCommandCount),
	})
	f.Fuzz(func(t *testing.T, commands []byte) {
		if len(commands) > 48 {
			commands = commands[:48]
		}
		runOutputProtocolModelOnDB(t, commands, migratedDB)
	})
}
