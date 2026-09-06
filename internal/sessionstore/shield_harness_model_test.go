package sessionstore

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type shieldHarnessCommand byte

const (
	shieldHarnessRaise shieldHarnessCommand = iota
	shieldHarnessDown
	shieldHarnessCreateChild
	shieldHarnessReplace
	shieldHarnessRestart
	shieldHarnessDuplicateRaise
)

type shieldHarnessModel struct {
	shieldsUp bool
	sessions  int
	commands  int
}

func (m *shieldHarnessModel) apply(command shieldHarnessCommand) {
	switch command {
	case shieldHarnessRaise, shieldHarnessDuplicateRaise:
		m.shieldsUp = true
		m.commands++
	case shieldHarnessDown:
		m.shieldsUp = false
		m.commands++
	case shieldHarnessCreateChild:
		m.sessions++
	case shieldHarnessReplace:
		m.sessions = 1
	case shieldHarnessRestart:
	}
}

type shieldHarnessProduction struct {
	t         *testing.T
	ctx       context.Context
	db        *sql.DB
	store     Store
	projectID int64
	rootID    int64
}

func newShieldHarnessProduction(t *testing.T) *shieldHarnessProduction {
	t.Helper()
	store, db, projectID := newTestStore(t)
	root, _, err := store.CreateManagerRoot(context.Background(), ManagerRootCreate{
		ProjectID: projectID, Model: "model", Attributes: map[string]any{"manager_id": "cli"},
		Name: "project", WorkDir: t.TempDir(),
	})
	require.NoError(t, err)

	return &shieldHarnessProduction{
		t: t, ctx: context.Background(), db: db, store: store, projectID: projectID, rootID: root.ID,
	}
}

func (p *shieldHarnessProduction) apply(command shieldHarnessCommand) {
	p.t.Helper()
	switch command {
	case shieldHarnessRaise:
		p.raise(false)
	case shieldHarnessDuplicateRaise:
		p.raise(true)
	case shieldHarnessDown:
		input, err := p.store.EnqueueInput(p.ctx, p.rootID, InputSourceUser, "/shieldsdown")
		require.NoError(p.t, err)
		_, err = p.store.ResolveShieldDown(p.ctx, input.ID, false)
		require.NoError(p.t, err)
	case shieldHarnessCreateChild:
		_, err := p.store.CreateSubagentSession(
			p.ctx, p.projectID, p.rootID, p.rootID, "general", "model", "",
		)
		require.NoError(p.t, err)
	case shieldHarnessReplace:
		replacement, _, err := p.store.ReplaceManagerRoot(p.ctx, p.rootID, "project", p.t.TempDir())
		require.NoError(p.t, err)
		p.rootID = replacement.ID
	case shieldHarnessRestart:
		p.store = NewStore(p.db)
	}
}

func (p *shieldHarnessProduction) raise(duplicate bool) {
	input, err := p.store.EnqueueInput(p.ctx, p.rootID, InputSourceUser, "/shieldsup")
	require.NoError(p.t, err)
	raise, first, err := p.store.BeginShieldRaise(p.ctx, input.ID, false, true)
	require.NoError(p.t, err)
	if duplicate {
		replayed, second, err := p.store.BeginShieldRaise(p.ctx, input.ID, false, true)
		require.NoError(p.t, err)
		assert.Equal(p.t, raise, replayed)
		assert.Equal(p.t, first.OutputID, second.OutputID)
	}
	if raise.Changed {
		_, err = p.store.CompleteShieldRaise(p.ctx, p.rootID, input.ID)
		require.NoError(p.t, err)
	}
}

func (p *shieldHarnessProduction) assertMatches(model *shieldHarnessModel, step int) {
	p.t.Helper()
	rows, err := p.db.QueryContext(p.ctx, `SELECT shields_up FROM sessions
		WHERE id = ? OR root_id = ? ORDER BY id`, p.rootID, p.rootID)
	require.NoError(p.t, err)
	defer rows.Close()

	count := 0
	for rows.Next() {
		var shieldsUp bool
		require.NoError(p.t, rows.Scan(&shieldsUp))
		assert.Equal(p.t, model.shieldsUp, shieldsUp, "step %d session %d", step, count)
		count++
	}
	require.NoError(p.t, rows.Err())
	assert.Equal(p.t, model.sessions, count, "step %d tree size", step)

	var handled, terminal int
	require.NoError(p.t, p.db.QueryRowContext(p.ctx, `SELECT COUNT(*) FROM session_inbox
		WHERE resolution_reason IN ('shieldsup', 'shieldsdown') AND state = 'handled'`).Scan(&handled))
	require.NoError(p.t, p.db.QueryRowContext(p.ctx, `SELECT COUNT(*) FROM session_outbox
		WHERE source_key LIKE '%:shieldsup:completed' OR source_key LIKE '%:shieldsdown:completed'`).Scan(&terminal))
	assert.Equal(p.t, model.commands, handled, "step %d handled commands", step)
	assert.Equal(p.t, model.commands, terminal, "step %d terminal command outputs", step)
}

func TestShieldHarnessModel_InheritanceReplayReplacementAndRestart(t *testing.T) {
	commands := []shieldHarnessCommand{
		shieldHarnessCreateChild,
		shieldHarnessRaise,
		shieldHarnessCreateChild,
		shieldHarnessRestart,
		shieldHarnessDuplicateRaise,
		shieldHarnessDown,
		shieldHarnessReplace,
		shieldHarnessRestart,
		shieldHarnessCreateChild,
	}
	model := &shieldHarnessModel{sessions: 1}
	production := newShieldHarnessProduction(t)

	for step, command := range commands {
		model.apply(command)
		production.apply(command)
		production.assertMatches(model, step)
	}
}
