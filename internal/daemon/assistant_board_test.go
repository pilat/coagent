package daemon

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/assistant"
	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/transcript"
)

func TestAssistantCatalogAndRead(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	work, err := h.store.CreateSession(h.ctx, h.projectID, "fake-model", "", managerAttrs(scenarioManagerID))
	require.NoError(t, err)
	in, err := h.store.Enqueue(
		h.ctx,
		sessionstore.Input{SessionID: work.ID, Source: sessionstore.InputSourceUser, Content: "real user"},
	)
	require.NoError(t, err)
	require.NoError(t, h.store.WithTx(h.ctx, func(tx *sql.Tx) error {
		return assistant.RecordSendTx(
			h.ctx,
			tx,
			assistant.Send{
				InputID:            in.Input.ID,
				AssistantSessionID: root.ID,
				SessionID:          work.ID,
				CreatedAt:          time.Now().UTC(),
			},
		)
	}))
	_, err = h.store.Commit(h.ctx, sessionstore.Commit{
		SessionID: work.ID,
		Accept: []sessionstore.Accept{
			{InputID: in.Input.ID, State: sessionstore.InputStateAccepted, Content: "real user", LinkRef: -1},
		},
		Messages: []*transcript.Message{
			{Role: "user", Content: "host nudge"},
			{Role: "assistant", Content: "answer\n" + strings.Repeat("я", 600)},
		},
	})
	require.NoError(t, err)
	require.NoError(t, h.store.UpdateSessionStatus(h.ctx, work.ID, sessionstore.SessionStatusCompleted))
	for _, draft := range []sessionstore.OutputDraft{
		{SessionID: work.ID, Type: sessionstore.OutputMessagePersistent, Content: "model answer\n🤖 footer", SourceKey: "message:123"},
		{SessionID: work.ID, Type: sessionstore.OutputMessagePersistent, Content: "receipt", SourceKey: "input:123"},
	} {
		draft.Fingerprint = sessionstore.OutputFingerprint(draft.Type, draft.Content, draft.SessionID, draft.Attributes)
		_, err = h.store.EnqueueOutput(h.ctx, draft)
		require.NoError(t, err)
	}
	_, err = h.store.CreateSession(h.ctx, h.projectID, "fake-model", "", managerAttrs("another-manager"))
	require.NoError(t, err)
	a := &assistantSessions{daemon: h.mgr, root: root.ID, owner: scenarioManagerID}
	projects, err := a.Catalog(h.ctx, time.Time{})
	require.NoError(t, err)
	var cards []assistant.Card
	for _, project := range projects {
		cards = append(cards, project.Sessions...)
	}
	require.Len(t, cards, 1)
	assert.Equal(t, work.ID, cards[0].ID)
	assert.Equal(t, "model answer", cards[0].LastLine)
	assert.Equal(t, "done", cards[0].Status)
	assert.True(t, cards[0].Unread)
	text, err := a.Read(h.ctx, work.ID, 60)
	require.NoError(t, err)
	assert.Contains(t, text, "user: real user")
	assert.Contains(t, text, "assistant: answer")
	assert.NotContains(t, text, "host nudge")
	assert.Contains(t, text, "answer\n"+strings.Repeat("я", 600), "a message is shown whole, not split into lines")
	assert.NotContains(t, text, "[…]")
	_, err = a.Read(h.ctx, root.ID, 1)
	require.Error(t, err)
}

func TestAssistantReadKeepsNewestWhole(t *testing.T) {
	t.Parallel()
	old, middle, newest := strings.Repeat("a", 30), strings.Repeat("b", 30), strings.Repeat("c", 30)
	assert.Equal(t, middle+"\n\n"+newest, newestWithin([]string{old, middle, newest}, 70))
	assert.Equal(t, "cccccc\n[…]\nccc", newestWithin([]string{newest}, 14), "only a lone oversized entry is cut")
}

func TestAssistantProjectPathArg(t *testing.T) {
	t.Parallel()
	home, err := coagenthome.UserHome()
	require.NoError(t, err)
	path, ok := projectPathArg("~/projects/pilat/coagent")
	assert.True(t, ok)
	assert.Equal(t, filepath.Join(home, "projects/pilat/coagent"), path)
	path, ok = projectPathArg("/srv/repo/")
	assert.True(t, ok)
	assert.Equal(t, "/srv/repo", path)
	_, ok = projectPathArg("coagent")
	assert.False(t, ok)
}
