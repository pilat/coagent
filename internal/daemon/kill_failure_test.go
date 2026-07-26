package daemon

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pilat/coagent/internal/logger"
)

// TestInjectCompletion_ReadErrorIsReturned: without the link nothing is
// injected; the caller gets a real failure instead of a log-only pseudo-success.
func TestInjectCompletion_ReadErrorIsLoud(t *testing.T) {
	h := newLedgerHarness(t)
	defer h.shutdown()

	before := h.parentMessages(h.parentID)

	link, linkErr := h.links.GetLink(h.ctx, h.childID)
	require.NoError(t, linkErr)
	require.NotNil(t, link)
	h.flaky.failGetLink(1, h.childID)
	err := h.mgr.injectBackgroundCompletion(h.ctx, &mockSession{}, h.childID, link.ActivationSeq)

	require.ErrorContains(t, err, "load background completion link")
	assert.Len(t, h.parentMessages(h.parentID), len(before), "the parent transcript is untouched")
}

// TestCascadeKill_ListErrorIsLoud: a failed listing means part of the tree stays
// alive, which must not look like "no descendants to kill".
func TestCascadeKill_ListErrorIsLoud(t *testing.T) {
	h := newLedgerHarness(t)
	defer h.shutdown()

	h.flaky.listPendingFail = true

	core, logs := observer.New(zap.ErrorLevel)
	ctx := logger.ToContext(h.ctx, zap.New(core))

	h.mgr.cascadeKillChildren(ctx, h.parentID, 0, time.Time{})

	assert.NotEmpty(t, logs.FilterMessage("cascade_list_children").All())

	rec, err := h.sessStore.GetSession(h.ctx, h.childID)
	require.NoError(t, err)
	assert.Nil(t, rec.KilledAt, "no child was killed")
	assert.Zero(t, h.flaky.markTerminalAttempts())
}

// TestKillSubagent_TerminalMarkRetries: the terminal mark must land before
// MarkSessionKilled, which is what makes the child invisible to the sweep.
func TestKillSubagent_TerminalMarkRetries(t *testing.T) {
	h := newLedgerHarness(t)
	defer h.shutdown()

	h.flaky.markTerminalFailN = 2

	h.mgr.killSubagent(h.ctx, h.childID, time.Time{})

	assert.Equal(t, 3, h.flaky.markTerminalAttempts())

	link, err := h.links.GetLink(h.ctx, h.childID)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.Equal(t, LinkStateKilled, link.State)

	rec, err := h.sessStore.GetSession(h.ctx, h.childID)
	require.NoError(t, err)
	assert.NotNil(t, rec.KilledAt, "killed_at is stamped only after the link is terminal")
}

// TestKillSubagent_TerminalMarkExhausted: killed_at plus a non-terminal link is
// the one combination no recovery path can see — the sweep filters on both — so
// the session writes are skipped and the child stays resumable.
func TestKillSubagent_TerminalMarkExhausted(t *testing.T) {
	h := newLedgerHarness(t)
	defer h.shutdown()

	h.flaky.markTerminalFailN = -1

	core, logs := observer.New(zap.ErrorLevel)
	ctx := logger.ToContext(h.ctx, zap.New(core))

	h.mgr.killSubagent(ctx, h.childID, time.Time{})

	assert.Equal(t, linkTerminalAttempts, h.flaky.markTerminalAttempts())
	assert.NotEmpty(t, logs.FilterMessage("kill_link_terminal").All())

	rec, err := h.sessStore.GetSession(h.ctx, h.childID)
	require.NoError(t, err)
	assert.Nil(t, rec.KilledAt, "killed_at must not be stamped on a non-terminal link")

	running, err := h.links.ListRunningChildLinks(h.ctx)
	require.NoError(t, err)
	assert.True(t,
		slices.ContainsFunc(running, func(l SubagentLink) bool { return l.ChildID == h.childID }),
		"the sweep can still recover it",
	)
}

// TestCascadeKill_RetryBudgetIsSharedAcrossTree: the walk is sequential and Kill
// waits on it, so a per-node retry budget would multiply by the tree size.
func TestCascadeKill_RetryBudgetIsSharedAcrossTree(t *testing.T) {
	h := newLedgerHarness(t)
	defer h.shutdown()

	for _, callID := range []string{"c2", "c3"} {
		childID, err := h.sessStore.CreateSubagentSession(
			h.ctx, h.projectID, h.parentID, h.parentID, "general", "fake-model", "",
		)
		require.NoError(t, err)
		require.NoError(t, h.links.InsertSubagentLink(h.ctx, SubagentLink{
			ParentID: h.parentID, ChildID: childID, TaskCallID: callID,
		}))
	}

	h.flaky.markTerminalFailN = -1

	start := time.Now()
	h.mgr.cascadeKillChildren(h.ctx, h.parentID, 0, time.Now().Add(-time.Second))
	elapsed := time.Since(start)

	// One attempt per child, not linkTerminalAttempts per child.
	assert.Equal(t, 3, h.flaky.markTerminalAttempts())
	assert.Less(t, elapsed, 200*time.Millisecond, "an exhausted budget stops the retries sleeping")
}
