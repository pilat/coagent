package session

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/sessionstore"
)

func newAttachmentsStore(t *testing.T) (*sql.DB, sessionstore.RuntimeStore, int64) {
	t.Helper()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "session.db")
	db, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(ctx, db, dbPath))

	result, err := db.ExecContext(
		ctx, `INSERT INTO projects (work_dir, name) VALUES (?, ?)`, t.TempDir(), "test",
	)
	require.NoError(t, err)
	projectID, err := result.LastInsertId()
	require.NoError(t, err)

	store := sessionstore.NewStore(db)
	record, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "telegram"})
	require.NoError(t, err)

	return db, store, record.ID
}

var demoRefs = []llmwire.ImageRef{
	{Path: "/tmp/coagent-a.png", Mime: llmwire.MimeImagePng, Size: 4096},
	{Path: "/tmp/coagent-b.jpg", Mime: llmwire.MimeImageJpeg, Size: 8192},
}

// appendImageToolResult commits an assistant call plus its image-bearing tool
// result through the ordinary persistence path.
func appendImageToolResult(ctx context.Context, t *testing.T, ms *messageStore, cid string) {
	t.Helper()

	require.NoError(t, ms.addAssistantMessage(
		ctx,
		&llmwire.Response{Text: "step", ToolCalls: []llmwire.ToolCall{{ID: cid, Name: "read"}}},
	))

	msg := llmwire.Message{
		Role:       llmwire.RoleTool,
		Content:    "[/tmp/coagent-a.png]\nimage loaded",
		ToolCallID: cid,
		ToolName:   "read",
		Images:     demoRefs,
	}

	if ms.store == nil {
		ms.messages = append(ms.messages, msg)

		return
	}

	stored, err := storedMessage(&msg)
	require.NoError(t, err)

	dbID, err := ms.store.InsertMessage(ctx, ms.sessID, stored)
	require.NoError(t, err)

	msg.DBID = dbID
	ms.mu.Lock()
	ms.messages = append(ms.messages, msg)
	ms.mu.Unlock()
}

// TestAttachments_SurviveRestart is the append→restart→reload protocol case:
// refs persisted on role-tool rows must come back byte-identical in order.
func TestAttachments_SurviveRestart(t *testing.T) {
	ctx := context.Background()
	_, store, sessionID := newAttachmentsStore(t)
	ms := newMessageStore(store, sessionID)

	appendImageToolResult(ctx, t, ms, "call-1")

	reloaded := newMessageStore(store, sessionID)
	require.NoError(t, reloaded.reloadMessages(ctx))

	got := reloaded.getMessages()
	require.Len(t, got, 2)
	require.Equal(t, llmwire.RoleTool, got[1].Role)
	assert.Equal(t, demoRefs, got[1].Images, "refs must survive restart in original order")
	assert.Empty(t, got[0].Images, "non-tool rows carry no refs")
}

func TestEstimateTokens_ImageDeltaBounded(t *testing.T) {
	base := []llmwire.Message{
		{Role: llmwire.RoleUser, Content: strings.Repeat("x", 400)},
	}
	withRef := func(size int64) []llmwire.Message {
		return []llmwire.Message{
			base[0],
			{
				Role: llmwire.RoleTool, Content: "img", ToolName: "read",
				Images: []llmwire.ImageRef{{Path: "/tmp/a.png", Mime: llmwire.MimeImagePng, Size: size}},
			},
		}
	}

	assert.Equal(t, 0, estimateTokens(withRef(0))-estimateTokens(base), "Size=0 is legal and free-ish")
	assert.Equal(t, 1, estimateTokens(withRef(5))-estimateTokens(base), "small Size charges Size/4")

	delta := estimateTokens(withRef(1<<30)) - estimateTokens(base)
	assert.Equal(t, 8192, delta, "huge files cap at the ceiling instead of triggering spurious compaction")
}
