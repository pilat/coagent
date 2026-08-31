package sessionstore

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/transcript"
)

func insertUsageRow(
	t *testing.T,
	store Store,
	sessionID int64,
	prompt, completion int,
	cost float64,
) int64 {
	t.Helper()

	usage, err := json.Marshal(llmwire.MessageUsage{PromptTokens: prompt, CompletionTokens: completion})
	require.NoError(t, err)

	id, err := store.InsertMessage(context.Background(), sessionID, &transcript.Message{
		Role:    llmwire.RoleAssistant,
		Content: "work",
		CostUSD: cost,
		Usage:   usage,
	})
	require.NoError(t, err)

	return id
}

// TestGetSessionTreeUsage sums the root's own messages, its subagents', and
// compacted rows — while ignoring an unrelated root on the same machine.
func TestGetSessionTreeUsage(t *testing.T) {
	store, _, projectID := newTestStore(t)
	ctx := context.Background()

	root, err := store.CreateSession(ctx, projectID, "m", "medium", nil)
	require.NoError(t, err)

	sub, err := store.CreateSubagentSession(ctx, projectID, root.ID, root.ID, "explore", "m", "medium")
	require.NoError(t, err)

	// Root's own: one active + one that gets compacted (still counted).
	insertUsageRow(t, store, root.ID, 1000, 100, 0.05)
	compactedID := insertUsageRow(t, store, root.ID, 2000, 200, 0.10)
	require.NoError(t, store.MarkCompacted(ctx, []int64{compactedID}))

	// Subagent's messages count too.
	insertUsageRow(t, store, sub, 500, 50, 0.02)

	// A second, unrelated root must not be swept in.
	other, err := store.CreateSession(ctx, projectID, "m", "medium", nil)
	require.NoError(t, err)
	insertUsageRow(t, store, other.ID, 9999, 999, 9.99)

	prompt, completion, cost, err := store.GetSessionTreeUsage(ctx, root.ID)
	require.NoError(t, err)

	assert.Equal(t, 3500, prompt, "1000 (root active) + 2000 (root compacted) + 500 (subagent)")
	assert.Equal(t, 350, completion, "100 + 200 + 50")
	assert.InDelta(t, 0.17, cost, 1e-9, "0.05 + 0.10 + 0.02")
}

// TestGetSessionTreeUsage_EmptyTree returns zeros, not an error, for a root with
// no usage-bearing messages.
func TestGetSessionTreeUsage_EmptyTree(t *testing.T) {
	store, _, projectID := newTestStore(t)
	ctx := context.Background()

	root, err := store.CreateSession(ctx, projectID, "m", "medium", nil)
	require.NoError(t, err)

	prompt, completion, cost, err := store.GetSessionTreeUsage(ctx, root.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, prompt)
	assert.Equal(t, 0, completion)
	assert.InDelta(t, 0.0, cost, 1e-9)
}
