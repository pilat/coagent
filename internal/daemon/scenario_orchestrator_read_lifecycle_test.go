package daemon

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
)

func TestOrchestratorReadSessionAcrossClearAndKill(t *testing.T) {
	for _, byNewID := range []bool{false, true} {
		name := "old id"
		if byNewID {
			name = "new id"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, harnessOptions{respond: readTurnRespond})
			h.startInboxWake()
			caller := modeRoot(t, h)
			target := h.createRoot(managerAttrs("owner"))
			h.reply(target, "first turn", "answer: first turn")
			readWithoutTargetChanges(t, h, caller, target)
			h.reply(target, "unread turn", "answer: unread turn")
			replacement := clearReadTarget(t, h, target)
			requested := target
			if byNewID {
				requested = replacement
			}
			page := readWithoutTargetChanges(t, h, caller, requested)
			assert.Equal(t, requested, page.Requested)
			assert.Equal(t, replacement, page.ID)
			require.Equal(t, []string{"answer", "notice"}, readKinds(page))
			assert.Equal(t, "answer: unread turn", page.Rows[0].Content)
			assert.Equal(t, "Session cleared.", page.Rows[1].Content)
			assertReadRekeyed(t, h, caller, replacement)
			h.reply(replacement, "new turn", "answer: new turn")
			page = readWithoutTargetChanges(t, h, caller, replacement)
			require.Equal(t, []string{"answer"}, readKinds(page))
			assert.Equal(t, "answer: new turn", page.Rows[0].Content)
			assertKilledRead(t, h, caller, replacement)
		})
	}
}

func TestOrchestratorReadPositionSurvivesRestart(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: readTurnRespond})
	h.startInboxWake()
	caller := modeRoot(t, h)
	target := h.createRoot(managerAttrs("owner"))
	h.reply(target, "before restart", "answer: before restart")
	page := readWithoutTargetChanges(t, h, caller, target)
	require.Equal(t, []string{"answer"}, readKinds(page))
	h = h.restart(readTurnRespond)
	h.startInboxWake()
	h.reply(target, "after restart", "answer: after restart")
	page = readWithoutTargetChanges(t, h, caller, target)
	require.Equal(t, []string{"answer"}, readKinds(page))
	assert.Equal(t, "answer: after restart", page.Rows[0].Content)
	assert.Zero(t, page.Skipped)
	assert.Empty(t, readWithoutTargetChanges(t, h, caller, target).Rows)
}

func readTurnRespond(_ string, messages []llmwire.Message) *llmwire.Response {
	for _, marker := range []string{"first turn", "unread turn", "new turn", "before restart", "after restart"} {
		if strings.Contains(lastUserText(messages), marker) {
			return textReply("answer: " + marker)
		}
	}
	return textReply("unscripted read turn")
}

func clearReadTarget(t *testing.T, h *harness, target int64) int64 {
	t.Helper()
	h.say(target, "/clear")
	var replacement int64
	h.waitUntil("read target replacement", func() bool {
		var err error
		replacement, err = h.store.ResolveReplacement(h.ctx, target)
		return err == nil && replacement != target
	})
	h.waitUntil("replacement clear notice", func() bool {
		return h.countOutbox(replacement, func(content string) bool { return content == "Session cleared." }) == 1 &&
			h.session(target).KilledAt != nil && !h.mgr.HasActiveLoop(target)
	})
	return replacement
}

func assertReadRekeyed(t *testing.T, h *harness, caller, replacement int64) {
	t.Helper()
	var count int
	var target int64
	require.NoError(t, h.db.QueryRowContext(h.ctx,
		`SELECT COUNT(*), target_id FROM session_watches WHERE watcher_id = ?`, caller).Scan(&count, &target))
	assert.Equal(t, 1, count)
	assert.Equal(t, replacement, target)
}

func assertKilledRead(t *testing.T, h *harness, caller, target int64) {
	t.Helper()
	h.say(target, "/kill")
	h.waitUntil("read target closed", func() bool {
		for _, row := range h.outbox(target) {
			if row.Type == "session_closed" {
				return true
			}
		}
		return false
	})
	page := readWithoutTargetChanges(t, h, caller, target)
	require.Equal(t, []string{"notice", "closed"}, readKinds(page))
	assert.Equal(t, "killed", page.State)
	assert.Contains(t, page.Rows[1].Content, "Session killed.")
}
