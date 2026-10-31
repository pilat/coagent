package daemon

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/projectpath"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

func TestAssistantOpenFailureLeavesNothing(t *testing.T) {
	for _, worktree := range []string{"", "atomic-wt"} {
		name := "plain project"
		if worktree != "" {
			name = "worktree"
		}

		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			repo := assistantWorktreeRepository(t)
			worktrees := t.TempDir()
			h := newHarness(t, harnessOptions{
				configure: func(cfg *config.Config) {
					withKnownModels([]string{"fake-model"})(cfg)
					cfg.UnifiedConfig.WorktreesRoot = worktrees
				},
			})
			h.db.SetMaxOpenConns(1) // TEMP triggers belong to one SQLite connection.
			root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
			require.NoError(t, err)
			_, err = h.store.GetOrCreateNamedProject(h.ctx, repo, "source")
			require.NoError(t, err)
			_, err = h.db.ExecContext(h.ctx, `CREATE TEMP TRIGGER fail_first_instruction BEFORE INSERT ON session_inbox
				WHEN NEW.raw_content LIKE '%atomic-open-marker%'
				BEGIN SELECT RAISE(ABORT, 'injected'); END;`)
			require.NoError(t, err)

			const liveRoots = `SELECT count(*) FROM sessions
				WHERE json_extract(attributes, '$.manager_id') = ? AND COALESCE(parent_id, 0) = 0
				AND agent_type <> 'assistant' AND killed_at IS NULL AND status <> 'killed'`
			var before int
			require.NoError(t, h.db.QueryRowContext(h.ctx, liveRoots, scenarioManagerID).Scan(&before))

			_, err = assistantFor(h, root.ID).Open(h.ctx,
				"source", "atomic-open-marker instruction", "atomic open title", worktree)
			require.ErrorContains(t, err, "injected")

			var after, titled int
			require.NoError(t, h.db.QueryRowContext(h.ctx, liveRoots, scenarioManagerID).Scan(&after))
			assert.Equal(t, before, after, "a failed first instruction must not leave a live root")
			require.NoError(t, h.db.QueryRowContext(h.ctx,
				`SELECT count(*) FROM sessions WHERE json_extract(attributes, '$.title') = ?`,
				"atomic open title").Scan(&titled))
			assert.Zero(t, titled, "root creation must roll back with its first instruction")

			if worktree != "" {
				assert.NoDirExists(t, projectpath.WorktreePath(worktrees, repo, worktree))
				branches, err := exec.CommandContext(t.Context(), "git", "-C", repo,
					"branch", "--list", worktree).CombinedOutput()
				require.NoError(t, err, "%s", branches)
				assert.Empty(t, string(branches))
			}
		})
	}
}

func TestAssistantOpenRejectsCommandsBeforeCreation(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	_, err = h.store.GetOrCreateNamedProject(h.ctx, t.TempDir(), "source")
	require.NoError(t, err)

	for _, command := range []string{"/status", "/help", "/schedules", "/compact", "/compact extra", "/stop", "/clear", "/kill"} {
		t.Run(command, func(t *testing.T) {
			_, err := assistantFor(h, root.ID).Open(h.ctx, "source", command, "command work", "atomic-wt")
			require.ErrorContains(t, err, "open needs an instruction; send commands to an existing session")
			var roots int
			require.NoError(t, h.db.QueryRowContext(h.ctx,
				`SELECT count(*) FROM sessions WHERE agent_type <> 'assistant'`).Scan(&roots))
			assert.Zero(t, roots)
		})
	}
}

func TestAssistantOpenDuringShutdownKeepsDurableInstruction(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	h.shutdown()

	card, err := assistantFor(h, root.ID).Open(h.ctx, "shutdown-work", "continue after boot", "durable work")
	require.NoError(t, err)
	record, err := h.store.GetSession(h.ctx, card.ID)
	require.NoError(t, err)
	assert.Nil(t, record.KilledAt)
	assert.Equal(t, sessionstore.SessionStatusActive, record.Status)
	assert.False(t, h.mgr.HasActiveLoop(card.ID))
	pending, err := h.store.ListPending(h.ctx, card.ID)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, "continue after boot", pending[0].RawContent)
	assert.Equal(t, "assistant", pending[0].Attributes["from"])
	assert.EqualValues(t, root.ID, pending[0].Attributes[sessionstore.ObserverSessionIDAttribute])
	recoverable, err := h.store.ListSessionsWithRecoverableInput(h.ctx)
	require.NoError(t, err)
	assert.Contains(t, recoverable, card.ID)
}

func TestAssistantOpenStartFailureKillsRootAndRemovesWorktree(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := assistantWorktreeRepository(t)
	worktrees := t.TempDir()
	var flaky *flakyLinkStore
	h := newHarness(t, harnessOptions{
		configure: func(cfg *config.Config) {
			withKnownModels([]string{"fake-model"})(cfg)
			cfg.UnifiedConfig.WorktreesRoot = worktrees
		},
		links: func(inner subagent.Store) subagent.Store {
			flaky = newFlakyLinkStore(inner)
			return flaky
		},
	})
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	_, err = h.store.GetOrCreateNamedProject(h.ctx, repo, "source")
	require.NoError(t, err)
	flaky.failGetLink(1, 0)

	_, err = assistantFor(h, root.ID).Open(h.ctx, "source", "first instruction", "failed launch", "atomic-wt")
	require.ErrorIs(t, err, errLinkRead)
	var id int64
	require.NoError(t, h.db.QueryRowContext(h.ctx,
		`SELECT id FROM sessions WHERE json_extract(attributes, '$.title') = 'failed launch'`).Scan(&id))
	record, err := h.store.GetSession(h.ctx, id)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.SessionStatusKilled, record.Status)
	assert.NotNil(t, record.KilledAt)
	var state string
	require.NoError(t, h.db.QueryRowContext(h.ctx,
		`SELECT state FROM session_inbox WHERE session_id = ?`, id).Scan(&state))
	assert.Equal(t, string(sessionstore.InputStateCancelled), state)
	var closed int
	require.NoError(t, h.db.QueryRowContext(h.ctx,
		`SELECT count(*) FROM session_outbox WHERE session_id = ? AND type = 'session_closed'`, id).Scan(&closed))
	assert.Equal(t, 1, closed)
	assert.NoDirExists(t, projectpath.WorktreePath(worktrees, repo, "atomic-wt"))
	branches, err := exec.CommandContext(t.Context(), "git", "-C", repo,
		"branch", "--list", "atomic-wt").CombinedOutput()
	require.NoError(t, err, "%s", branches)
	assert.Empty(t, string(branches))
}
