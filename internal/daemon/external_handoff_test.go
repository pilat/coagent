package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/transcript"
)

type deliveringChildLinks struct {
	subagent.Store
	transactions subagent.Transactions
	link         subagent.Link
	delivered    bool
}

func (s *deliveringChildLinks) ListPendingChildLinks(ctx context.Context, parentID int64) ([]subagent.Link, error) {
	if parentID == s.link.ParentID && !s.delivered {
		won, err := s.transactions.DeliverCompletion(ctx, s.link, "child finished during ownership capture")
		if err != nil {
			return nil, fmt.Errorf("deliver ownership handoff: %w", err)
		}

		if !won {
			return nil, fmt.Errorf("ownership handoff did not commit for child %d", s.link.ChildID)
		}

		s.delivered = true
	}

	links, err := s.Store.ListPendingChildLinks(ctx, parentID)
	if err != nil {
		return nil, fmt.Errorf("read handed-off child links: %w", err)
	}

	return links, nil
}

func TestPendingExternalCallsRetainsAtomicChildHandoff(t *testing.T) {
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "handoff.db")
	db, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(ctx, db, dbPath))
	sessions := sessionstore.NewStore(db)
	links := subagent.NewStore(db)
	transactions := subagent.NewTransactions(db, sessions)
	projectID := testProject(t, sessions, t.TempDir())
	parent, err := sessions.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)
	encoded, err := json.Marshal([]llmwire.ToolCall{{ID: taskCallID, Name: tool.IDTask}})
	require.NoError(t, err)
	_, err = sessions.Commit(ctx, sessionstore.Commit{
		SessionID: parent.ID,
		Messages:  []*transcript.Message{{Role: llmwire.RoleAssistant, ToolCalls: encoded}},
	})
	require.NoError(t, err)
	childID, err := transactions.Create(ctx, subagent.Create{
		ProjectID: projectID, ParentID: parent.ID, RootID: parent.ID,
		AgentType: "general", Model: "fake-model", TaskCallID: taskCallID,
		Blocking: true, Depth: 1, State: subagent.StateSpawned,
	})
	require.NoError(t, err)
	finalized, err := transactions.TryFinalizeActivation(
		ctx, childID, subagent.StateCompleted, "child finished", subagent.OutcomeCompleted,
	)
	require.NoError(t, err)
	require.True(t, finalized)
	link, err := links.GetLink(ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, link)
	handoff := &deliveringChildLinks{Store: links, transactions: transactions, link: *link}
	manager := &svc{store: sessions, links: handoff}

	owners, err := manager.pendingExternalCallsForSession(ctx, parent.ID)
	require.NoError(t, err)
	require.True(t, handoff.delivered)
	require.Equal(t, map[string]string{taskCallID: tool.IDTask}, owners)
	pendingLinks, err := links.ListPendingChildLinks(ctx, parent.ID)
	require.NoError(t, err)
	require.Empty(t, pendingLinks)
	pendingInputs, err := sessions.ListPending(ctx, parent.ID)
	require.NoError(t, err)
	require.Len(t, pendingInputs, 1)
	require.Equal(t, sessionstore.InputSourceCallResult, pendingInputs[0].Source)
	require.Equal(t, taskCallID, pendingInputs[0].Attributes["call_id"])
	require.Equal(t, tool.IDTask, pendingInputs[0].Attributes["tool_id"])
}
