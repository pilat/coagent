package daemon

import (
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/assistant"
	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestAssistantCatalogFiltersAndOldSessions(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	a := &assistantSessions{daemon: h.mgr, root: root.ID, owner: scenarioManagerID}
	hidden, err := h.store.GetOrCreateHiddenProject(h.ctx, t.TempDir())
	require.NoError(t, err)
	worktrees, err := coagenthome.Join(coagenthome.WorktreesDirName)
	require.NoError(t, err)
	worktree := filepath.Join(worktrees, "catalog-hidden")
	require.NoError(t, os.MkdirAll(worktree, 0o700))
	worktreeID, err := h.store.GetOrCreateProject(h.ctx, worktree)
	require.NoError(t, err)
	missing, err := h.store.GetOrCreateProject(h.ctx, filepath.Join(t.TempDir(), "missing"))
	require.NoError(t, err)
	old := catalogTestWork(t, h, root.ID, h.projectID, true)
	_, err = h.db.ExecContext(h.ctx, `UPDATE assistant_sends SET observed_at=CURRENT_TIMESTAMP WHERE session_id=?`, old)
	require.NoError(t, err)
	_, err = h.db.ExecContext(
		h.ctx,
		`UPDATE sessions SET updated_at = ?, context_baseline_prompt_tokens=40000 WHERE id=?`,
		time.Now().UTC().Add(-30*24*time.Hour),
		old,
	)
	require.NoError(t, err)
	user := catalogTestWork(t, h, root.ID, h.projectID, false)
	killed := catalogTestWork(t, h, root.ID, h.projectID, true)
	_, err = h.db.ExecContext(
		h.ctx,
		`UPDATE sessions SET killed_at=CURRENT_TIMESTAMP,status='killed' WHERE id=?`,
		killed,
	)
	require.NoError(t, err)
	projects, err := a.Catalog(h.ctx, time.Now().UTC())
	require.NoError(t, err)
	var cards []assistant.Card
	for _, project := range projects {
		assert.NotContains(t, []int64{hidden, worktreeID, missing}, project.ProjectID)
		cards = append(cards, project.Sessions...)
	}
	require.Len(t, cards, 1)
	assert.Equal(t, old, cards[0].ID)
	assert.Equal(t, 40000, cards[0].ContextTokens)
	assert.NotEqual(t, user, cards[0].ID)
	assert.NotEqual(t, killed, cards[0].ID)
}

func TestAssistantCatalogSelectsPinnedBeforeEnrichment(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	a := &assistantSessions{daemon: h.mgr, root: root.ID, owner: scenarioManagerID}
	var ids []int64
	for index := range 12 {
		id := catalogTestWork(t, h, root.ID, h.projectID, true)
		ids = append(ids, id)
		_, err = h.db.ExecContext(
			h.ctx,
			`UPDATE sessions SET updated_at=? WHERE id=?`,
			time.Now().UTC().Add(time.Duration(index)*time.Minute),
			id,
		)
		require.NoError(t, err)
	}
	_, err = h.db.ExecContext(
		h.ctx,
		`UPDATE assistant_sends SET observed_at=CURRENT_TIMESTAMP WHERE session_id!=?`,
		ids[2],
	)
	require.NoError(t, err)
	text := "waiting"
	require.NoError(t, h.store.WithTx(h.ctx, func(tx *sql.Tx) error {
		return assistant.SaveNotesTx(h.ctx, tx, root.ID, map[string]*string{fmtWait(ids[0]): &text}, time.Now().UTC())
	}))
	projects, err := a.Catalog(h.ctx, time.Now().UTC().Add(24*time.Hour), ids[1])
	require.NoError(t, err)
	var cards []assistant.Card
	for _, project := range projects {
		cards = append(cards, project.Sessions...)
	}
	require.Len(t, cards, 8)
	for _, id := range []int64{ids[0], ids[1], ids[2]} {
		found := false
		for _, card := range cards {
			found = found || card.ID == id
		}
		assert.True(t, found)
	}
}

func TestAssistantCatalogDuplicateNamesAndCost(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	projectsRoot, err := coagenthome.Join(coagenthome.ProjectsDirName)
	require.NoError(t, err)
	scratch := filepath.Join(projectsRoot, "catalog-same")
	require.NoError(t, os.MkdirAll(scratch, 0o700))
	first, err := h.store.GetOrCreateNamedProject(h.ctx, t.TempDir(), "same")
	require.NoError(t, err)
	second, err := h.store.GetOrCreateNamedProject(h.ctx, scratch, "same")
	require.NoError(t, err)
	work := catalogTestWork(t, h, root.ID, first, true)
	child, err := h.store.CreateSession(h.ctx, first, "fake-model", "", nil)
	require.NoError(t, err)
	_, err = h.db.ExecContext(h.ctx, `UPDATE sessions SET parent_id=?,root_id=? WHERE id=?`, work, work, child.ID)
	require.NoError(t, err)
	for _, row := range []struct {
		id   int64
		cost float64
	}{{work, .12}, {child.ID, .03}, {work, -1}, {work, 1e180}, {child.ID, 1e247}} {
		_, err = h.db.ExecContext(
			h.ctx,
			`INSERT INTO messages(session_id,role,content,cost_usd)VALUES(?,'assistant','cost',?)`,
			row.id,
			row.cost,
		)
		require.NoError(t, err)
	}
	a := &assistantSessions{daemon: h.mgr, root: root.ID, owner: scenarioManagerID}
	projects, err := a.Catalog(h.ctx, time.Now().UTC())
	require.NoError(t, err)
	matched := 0
	for _, project := range projects {
		if project.ProjectID != first && project.ProjectID != second {
			continue
		}
		matched++
		assert.NotEqual(t, "same", project.Key)
		path, ok := projectPathArg(project.Key)
		require.True(t, ok)
		id, _, _, err := a.resolveProject(h.ctx, project.Key)
		require.NoError(t, err)
		assert.Equal(t, project.ProjectID, id)
		if project.ProjectID == second {
			assert.Equal(t, scratch, path)
			assert.True(t, strings.HasPrefix(project.Place, "scratch "))
		}
		if project.ProjectID == first {
			require.Len(t, project.Sessions, 1)
			assert.InDelta(t, .15, project.Sessions[0].CostUSD, .00001)
		}
	}
	assert.Equal(t, 2, matched)
}

func TestAssistantOpenNoteFailureRollsBackFirstSend(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	_, err = h.db.ExecContext(h.ctx, `CREATE TRIGGER reject_session_note BEFORE INSERT ON assistant_notes
		WHEN NEW.key LIKE 'session.%' BEGIN SELECT RAISE(ABORT,'note unavailable'); END`)
	require.NoError(t, err)
	a := &assistantSessions{daemon: h.mgr, root: root.ID, owner: scenarioManagerID}
	_, err = a.Open(h.ctx, "note-failure", "must not run", "purpose")
	require.ErrorContains(t, err, "note unavailable")
	var sends, inputs int
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM assistant_sends`).Scan(&sends))
	require.NoError(
		t,
		h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM session_inbox WHERE raw_content='must not run'`).
			Scan(&inputs),
	)
	assert.Zero(t, sends)
	assert.Zero(t, inputs)
}

func catalogTestWork(t *testing.T, h *harness, root, project int64, touched bool) int64 {
	t.Helper()
	work, err := h.store.CreateSession(h.ctx, project, "fake-model", "", managerAttrs(scenarioManagerID))
	require.NoError(t, err)
	if !touched {
		return work.ID
	}
	in, err := h.store.Enqueue(
		h.ctx,
		sessionstore.Input{SessionID: work.ID, Source: sessionstore.InputSourceUser, Content: "work"},
	)
	require.NoError(t, err)
	require.NoError(t, h.store.WithTx(h.ctx, func(tx *sql.Tx) error {
		return assistant.RecordSendTx(
			h.ctx,
			tx,
			assistant.Send{
				InputID:            in.Input.ID,
				AssistantSessionID: root,
				SessionID:          work.ID,
				CreatedAt:          time.Now().UTC(),
			},
		)
	}))
	return work.ID
}

func fmtWait(id int64) string { return "wait." + strconv.FormatInt(id, 10) }
