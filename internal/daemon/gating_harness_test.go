package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

// gatingHarness is the subagent harness over a project WorkDir carrying
// .claude/agents, recording the tool schemas each session offered its model.
type gatingHarness struct {
	*subagentHarness

	schemas *schemaRecorder
}

// schemaRecorder collects, per session, the union of tool names offered to the
// model across every provider call.
type schemaRecorder struct {
	mu    sync.Mutex
	names map[int64]map[string]bool
}

// recordingLLM is the scripted client that reports what the session's registry
// produced — the boundary a user's model actually sees.
type recordingLLM struct {
	scriptedLLM

	rec *schemaRecorder

	mu        sync.Mutex
	sessionID int64
}

func (r *schemaRecorder) record(sessionID int64, schemas []llmwire.ToolSchema) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.names[sessionID] == nil {
		r.names[sessionID] = make(map[string]bool)
	}

	for _, s := range schemas {
		r.names[sessionID][s.Name] = true
	}
}

func (r *schemaRecorder) offered(sessionID int64) map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make(map[string]bool, len(r.names[sessionID]))
	for name := range r.names[sessionID] {
		out[name] = true
	}

	return out
}

func (c *recordingLLM) SetSessionID(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Children report "root:child"; the session's own id is the last segment.
	if idx := strings.LastIndex(id, ":"); idx >= 0 {
		id = id[idx+1:]
	}

	var parsed int64
	_, _ = fmt.Sscanf(id, "%d", &parsed)
	c.sessionID = parsed
}

func (c *recordingLLM) Chat(
	ctx context.Context,
	system string,
	msgs []llmwire.Message,
	tools []llmwire.ToolSchema,
	opts ...llmwire.ChatOption,
) (*llmwire.Response, error) {
	c.mu.Lock()
	sessionID := c.sessionID
	c.mu.Unlock()

	c.rec.record(sessionID, tools)

	return c.scriptedLLM.Chat(ctx, system, msgs, tools, opts...)
}

func newGatingHarness(
	t *testing.T,
	agents map[string]string,
	respond func(system string, msgs []llmwire.Message) *llmwire.Response,
) *gatingHarness {
	t.Helper()

	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	db, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(ctx, db, dbPath))

	store := NewStore(db)
	sessStore := sessionstore.NewStore(db)
	links := NewLinkStore(db)
	schedStore := schedule.NewStore(db)

	workDir := t.TempDir()
	writeProjectAgents(t, workDir, agents)

	rec := &schemaRecorder{names: make(map[int64]map[string]bool)}
	cfg := &config.Config{WorkDir: workDir, Model: "fake-model"}

	factory := session.NewFactoryWithOptions(
		cfg, nil, nil, sessStore, nil, nil, nil, nil, nil,
		session.WithLLMClientFactory(func(_ *config.Config) (llm.Client, error) {
			return &recordingLLM{scriptedLLM: scriptedLLM{respond: respond}, rec: rec}, nil
		}),
	)

	mgr := newSvc(
		factory, store, sessStore, sessStore, links, schedule.NewService(schedStore),
		func() string { return "fake-model" },
	)
	mgr.applier = NewConfigApplier(newTestConfigOps(t, dir), func() {})

	pid, err := store.GetOrCreateProject(ctx, workDir)
	require.NoError(t, err)

	return &gatingHarness{
		subagentHarness: &subagentHarness{
			t: t, mgr: mgr, sessStore: sessStore, links: links, schedStore: schedStore,
			projectID: pid, ctx: ctx,
		},
		schemas: rec,
	}
}

// newTestConfigOps gives the daemon a real config mutation layer over temp
// files, so config tools are registered on root sessions at all.
func newTestConfigOps(t *testing.T, dir string) configops.Service {
	t.Helper()

	configPath := filepath.Join(dir, "config.yaml")
	secretsPath := filepath.Join(dir, "secrets")
	require.NoError(t, os.WriteFile(configPath, []byte(toolConfig), 0o600))
	require.NoError(t, os.WriteFile(secretsPath, []byte(toolSecrets), 0o600))

	return configops.New(configPath, secretsPath)
}

func writeProjectAgents(t *testing.T, workDir string, agents map[string]string) {
	t.Helper()

	if len(agents) == 0 {
		return
	}

	dir := filepath.Join(workDir, ".claude", "agents")
	require.NoError(t, os.MkdirAll(dir, 0o755))

	for name, body := range agents {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600))
	}
}

func (h *gatingHarness) waitForLink(parentID int64, callID string) SubagentLink {
	h.t.Helper()

	var link *SubagentLink

	require.Eventually(h.t, func() bool {
		found, err := h.links.GetLinkByTaskCallID(h.ctx, parentID, callID)
		require.NoError(h.t, err)
		link = found

		return found != nil
	}, 5*time.Second, 10*time.Millisecond, "child link for %s", callID)

	return *link
}

// spawnTaskCall is a background task tool_call for the given project/built-in type.
func spawnTaskCall(callID, agentType, marker string) llmwire.ToolCall {
	return llmwire.ToolCall{
		ID:   callID,
		Name: tool.IDTask,
		Arguments: fmt.Appendf(nil,
			`{"prompt":%q,"description":"gating probe","subagent_type":%q,"background":true}`,
			marker+" probe the toolset", agentType,
		),
	}
}

// probeMissingTools calls each tool once, in order, then finishes. A tool the
// session never gained comes back as an unknown-tool error.
func probeMissingTools(msgs []llmwire.Message, prefix string, ids []string) *llmwire.Response {
	for i, id := range ids {
		if hasToolResultFor(msgs, id) {
			continue
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID: fmt.Sprintf("%s-probe-%d", prefix, i), Name: id, Arguments: []byte(`{}`),
		}}}
	}

	return &llmwire.Response{Text: prefix + " done"}
}

func (h *gatingHarness) assertUnknownTools(sessionID int64, ids []string) {
	h.t.Helper()

	msgs := h.parentMessages(sessionID)
	require.NoError(h.t, llm.ValidateToolPairing(msgs), "child transcript must stay provider-valid")

	for _, id := range ids {
		assert.Equal(h.t, 1, countToolResultsFor(msgs, id), "one result for the %q probe", id)
		assert.Contains(h.t, lastToolResultContent(msgs, id), "unknown tool: "+id)
	}
}

func assertNotOffered(t *testing.T, offered map[string]bool, ids []string) {
	t.Helper()

	for _, id := range ids {
		assert.NotContains(t, offered, id, "gated tool %q must not reach the model", id)
	}
}
