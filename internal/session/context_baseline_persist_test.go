package session

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/tool/builtin"
)

// A session resumed after a restart keeps the provider's own count: the
// projection is measured, not estimated, which is what /status needs to drop
// its tilde and what the compaction trigger should compare against (D8).
func TestNewWithOptions_InstallsPersistedBaselineForTheSameModel(t *testing.T) {
	s := resumeWithPersistedBaseline(t, "test-model", &sessionstore.ContextBaseline{
		Model: "test-model", PromptTokens: 150_000, MessageCount: 2,
	})

	base := s.loadContextBaseline()
	require.NotNil(t, base)
	assert.Equal(t, 150_000, base.promptTokens)

	size, estimated := s.projectContextSize()
	assert.False(t, estimated, "the projection is measured")
	assert.Equal(t, 150_000+estimateTokens(s.ms.getMessages()[2:]), size)
}

// A measurement belongs to one model's window and tokenizer; a session that
// boots under another model falls back to estimation.
func TestNewWithOptions_DiscardsPersistedBaselineForAnotherModel(t *testing.T) {
	s := resumeWithPersistedBaseline(t, "test-model", &sessionstore.ContextBaseline{
		Model: "other-model", PromptTokens: 150_000, MessageCount: 2,
	})

	assert.Nil(t, s.loadContextBaseline())

	_, estimated := s.projectContextSize()
	assert.True(t, estimated)
}

func resumeWithPersistedBaseline(t *testing.T, model string, baseline *sessionstore.ContextBaseline) *svc {
	t.Helper()

	resumeMessages := []llmwire.Message{
		{Role: llmwire.RoleUser, Content: "initial"},
		{Role: llmwire.RoleAssistant, Content: "ack"},
	}

	reg := tool.NewRegistry()
	reg.Register(builtin.NewLsTool("/tmp/project"))

	p := params{
		Config:    &config.Config{WorkDir: "/tmp/project", Model: model},
		LLMClient: &mockLLMClient{},
		TodoStore: todo.New(),
		Loader:    loader.New(),
		Registry:  reg,
	}

	sessionSvc, err := newWithOptions(context.Background(), p, options{
		ID:              7,
		ResumeMessages:  resumeMessages,
		ContextBaseline: baseline,
	})
	require.NoError(t, err)

	return sessionSvc.(*svc)
}

// The full durable protocol: a successful response persists the measurement,
// a compaction commit clears the row, and a crash between commit and the next
// success cannot resurrect a stale baseline.
func TestContextBaseline_CompactionClearsThePersistedRow(t *testing.T) {
	ctx := context.Background()

	db, store, sessionID := newBaselineTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	mockLLM := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: 32_000,
	}
	s := newCompactionTestSvc(mockLLM)
	s.store = store
	s.id = sessionID
	s.ms = newMessageStore(store, sessionID, nil)
	s.ms.setMessages(oversizedTranscript(32_000))

	s.recordContextBaseline(ctx, 150_000, 2, s.modelGeneration())

	var model string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT context_baseline_model FROM sessions WHERE id = ?`, sessionID).Scan(&model))
	assert.Equal(t, s.model, model, "the measurement persists on the session row")

	ok, err := s.compact(ctx, nil)
	require.NoError(t, err)
	require.True(t, ok)

	var tokens int
	require.NoError(t, db.QueryRowContext(
		ctx,
		`SELECT context_baseline_model, context_baseline_prompt_tokens FROM sessions WHERE id = ?`,
		sessionID,
	).Scan(&model, &tokens))
	assert.Empty(t, model, "the compaction commit clears the persisted baseline")
	assert.Zero(t, tokens)
}

func newBaselineTestDB(t *testing.T) (*sql.DB, sessionstore.RuntimeStore, int64) {
	t.Helper()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "session.db")
	db, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	require.NoError(t, migrate.Run(ctx, db, dbPath))

	result, err := db.ExecContext(
		ctx, `INSERT INTO projects (work_dir, name) VALUES (?, ?)`, t.TempDir(), "test",
	)
	require.NoError(t, err)

	projectID, err := result.LastInsertId()
	require.NoError(t, err)

	store := sessionstore.NewStore(db)
	record, err := store.CreateSession(ctx, projectID, "test-model", "", map[string]any{"manager_id": "telegram"})
	require.NoError(t, err)

	return db, store, record.ID
}
