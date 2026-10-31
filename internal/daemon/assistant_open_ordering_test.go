package daemon

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/projectpath"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

type assistantOpenCommitHookStore struct {
	Store
	afterCommit func()
	beforeKill  func(int64)
}

func TestAssistantOpenShutdownCancelsContextAfterCommit(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	h.mgr.store = &assistantOpenCommitHookStore{Store: h.mgr.store, afterCommit: func() {
		h.mgr.life.close()
		cancel()
	}}

	_, err = assistantFor(h, root.ID).Open(ctx, "shutdown-commit", "recover after boot", "committed work")
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorContains(t, err, "load opened session model", "only response rendering sees cancellation")
	var id int64
	require.NoError(t, h.db.QueryRowContext(h.ctx,
		`SELECT id FROM sessions WHERE json_extract(attributes, '$.title') = 'committed work'`).Scan(&id))
	record, err := h.store.GetSession(h.ctx, id)
	require.NoError(t, err)
	assert.Nil(t, record.KilledAt)
	pending, err := h.store.ListPending(h.ctx, id)
	require.NoError(t, err)
	assert.Len(t, pending, 1)
	recoverable, err := h.store.ListSessionsWithRecoverableInput(h.ctx)
	require.NoError(t, err)
	assert.Contains(t, recoverable, id)
	lockCtx, stop := context.WithTimeout(h.ctx, time.Second)
	defer stop()
	unlock, err := h.mgr.trees.lock(lockCtx, id)
	require.NoError(t, err, "shutdown must release the creation fence")
	unlock()
}

func TestAssistantOpenFailedStartDoesNotLeaveRunnerInRemovedWorktree(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := assistantWorktreeRepository(t)
	worktrees := t.TempDir()
	release := make(chan struct{})
	var flaky *flakyLinkStore
	h := newHarness(t, harnessOptions{
		configure: func(cfg *config.Config) {
			withKnownModels([]string{"fake-model"})(cfg)
			cfg.UnifiedConfig.WorktreesRoot = worktrees
		},
		links: func(inner subagent.Store) subagent.Store {
			flaky = newFlakyLinkStore(inner)
			flaky.getLinkFailOnly = 1
			return flaky
		},
		respond: func(_ string, _ []llmwire.Message) *llmwire.Response {
			<-release
			return textReply("finished")
		},
	})
	t.Cleanup(func() { closeOnce(release) })
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	_, err = h.store.GetOrCreateNamedProject(h.ctx, repo, "source")
	require.NoError(t, err)
	var id int64
	h.mgr.store = &assistantOpenCommitHookStore{Store: h.mgr.store, beforeKill: func(sessionID int64) {
		id = sessionID
		wakeCtx, cancel := context.WithTimeout(h.ctx, time.Second)
		defer cancel()
		// A committed wake must not admit a runner while failed-start cleanup owns the root.
		err := h.mgr.inputReady(wakeCtx, id)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.ErrorContains(t, err, "acquire session tree lock")
	}}

	_, err = assistantFor(h, root.ID).Open(h.ctx, "source", "work instruction", "wake before cleanup", "atomic-wt")
	require.ErrorIs(t, err, errLinkRead)
	record, err := h.store.GetSession(h.ctx, id)
	require.NoError(t, err)
	require.Equal(t, sessionstore.SessionStatusKilled, record.Status)
	assert.NoDirExists(t, projectpath.WorktreePath(worktrees, repo, "atomic-wt"))
	assert.False(t, h.mgr.HasActiveLoop(id), "failed-open cleanup must join work before removing its worktree")
	lockCtx, cancel := context.WithTimeout(h.ctx, time.Second)
	defer cancel()
	unlock, err := h.mgr.trees.lock(lockCtx, id)
	require.NoError(t, err, "failed startup must release the creation fence")
	unlock()
}

func (s *assistantOpenCommitHookStore) WithTx(ctx context.Context, fn func(*sql.Tx) error) error {
	if err := s.Store.WithTx(ctx, fn); err != nil {
		return err
	}

	if s.afterCommit != nil {
		s.afterCommit()
	}

	return nil
}

func (s *assistantOpenCommitHookStore) MarkSessionKilledWithOutput(
	ctx context.Context, id int64, cancelledProcesses int,
) (*sessionstore.OutputCommit, error) {
	if s.beforeKill != nil {
		s.beforeKill(id)
	}

	return s.Store.MarkSessionKilledWithOutput(ctx, id, cancelledProcesses)
}
