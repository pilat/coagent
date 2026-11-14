package daemon

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/configapply"
	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llm/llmtest"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/managercontrol"
	"github.com/pilat/coagent/internal/managerdiscovery"
	"github.com/pilat/coagent/internal/mcpstore"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/progressruntime"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/sessionbus"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/transcript"
)

func (h *harness) waitUntil(label string, cond func() bool) {
	h.t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if cond() {
			return
		}
		select {
		case <-ticker.C:
		case <-timer.C:
			h.t.Fatalf("timed out waiting for: %s", label)
		}
	}
}

// Duplicating compaction notices across the package boundary makes user-visible rewording break the scenarios.
const (
	noticeCompacting       = "🔄 Compacting context..."
	noticeCompacted        = "✅ Context compacted"
	noticeCompactionFailed = "❌ Compaction failed"
	noticeNothingToCompact = "Nothing to compact"
	noticeCompactDeferred  = "⏳ Compaction deferred until the session finishes waiting"
)

const contextSummaryPrefix = "[CONTEXT SUMMARY"

// The final checkpoint instruction distinguishes summarization from an ordinary user turn.
func isCompactionInstruction(msgs []llmwire.Message) bool {
	if len(msgs) == 0 {
		return false
	}
	last := msgs[len(msgs)-1]
	return strings.Contains(last.Content, "continuation checkpoint")
}

// The extra settled tool round remains summarizable while the blocking child's launch pair stays in the tail.
func blockingCompactRespond(release <-chan struct{}) func(string, []llmwire.Message) *llmwire.Response {
	return func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if isCompactionInstruction(msgs) {
			return textReply("## Goal\nspawn work\n## Progress\n- child ran\n## Context for Continuation\ncarry on")
		}
		if hasUserContaining(msgs, "CHILD_TASK") {
			if release != nil {
				<-release
			}
			return textReply("blocking child done: 7")
		}

		// Deferred compaction continues the activation; a repeated prompt must not spawn another child.
		if hasSummaryRow(msgs) || hasToolResultFor(msgs, "task") {
			return textReply("parent got the child result")
		}
		if !hasToolResultFor(msgs, "ls") {
			return callReply("ls-before-spawn", "ls", `{"path":"."}`)
		}
		return callReply(
			taskCallID, "task", `{"prompt":"CHILD_TASK do it","description":"c","subagent_type":"general"}`,
		)
	}
}

func providerSystemText(request providerRequest) string {
	for _, message := range request.Messages {
		if message.Role == llmwire.RoleSystem {
			var system string
			if err := json.Unmarshal(message.Content, &system); err != nil {
				panic(err)
			}
			return system
		}
	}
	return ""
}

func hasSummaryRow(msgs []llmwire.Message) bool {
	for _, m := range msgs {
		if strings.HasPrefix(m.Content, contextSummaryPrefix) {
			return true
		}
	}
	return false
}

// Direct Spawn fixtures need terminal children that never spawn descendants.
func trivialRespond(_ string, _ []llmwire.Message) *llmwire.Response {
	return textReply("done")
}

func lastAssistantTextDTO(msgs []llmwire.Message) string {
	for _, v := range slices.Backward(msgs) {
		m := v
		if m.Role == llmwire.RoleAssistant && len(m.ToolCalls) == 0 && m.Content != "" {
			return m.Content
		}
	}
	return ""
}

// closeOnce closes ch unless it is already closed (cleanup helper for hold channels).
func closeOnce(ch chan struct{}) {
	select {
	case <-ch:
	default:
		close(ch)
	}
}

func (h *harness) queueLen() int {
	return runnerWaitingCount(h.mgr.runners)
}

func newApplyDaemonWith(
	t *testing.T,
	dbPath, configDir string,
	respond func(system string, msgs []llmwire.Message) *llmwire.Response,
) *applyDaemon {
	t.Helper()
	h := newHarness(t, harnessOptions{dbPath: dbPath, respond: respond})
	ops := configops.New(filepath.Join(configDir, "config.yaml"), filepath.Join(configDir, "secrets"))
	h.mgr.applier = configapply.New(ops)
	return &applyDaemon{harness: h, ops: ops, restarts: h.mgr.applier.Restart()}
}

// Separate daemon images share durable state and config so restart-apply scenarios retain their real boundary.
type applyDaemon struct {
	*harness
	ops      configops.Service
	restarts <-chan struct{}
}

func newApplyConfigDir(t *testing.T) string {
	t.Helper()
	return newApplyConfigDirWith(t, toolConfig)
}

func newApplyConfigDirWith(t *testing.T, configYAML string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(configYAML), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "secrets"), []byte(toolSecrets), 0o600))
	return dir
}

func (d *applyDaemon) restartCount() int { return len(d.restarts) }

const configEditCandidate = `providers:
    work:
        driver: anthropic
        api_key: ${WORK_API_KEY}
models:
    - id: claude-opus-5
      provider: work
    - id: claude-sonnet-5
      provider: work
`

func mustQuoteJSON(s string) string {
	encoded, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// toolConfig is a valid starting point every config-tool test mutates from.
const toolConfig = `providers:
    work:
        driver: anthropic
        api_key: ${WORK_API_KEY}
models:
    - id: claude-sonnet-5
      provider: work
    - id: claude-opus-5
      provider: work
`

//nolint:gosec // fake credentials
const toolSecrets = "WORK_API_KEY=sk-ant-work-0000000000\n"

func newTestController(svc *Service) controllerapi.ManagerControllerFactory {
	cfg := &config.Config{}
	var outputs *sessionstore.Store
	if svc != nil {
		outputs, _ = svc.store.(*sessionstore.Store)
	}
	return managercontrol.New(
		svc, outputs, managerdiscovery.New(outputs, cfg, nil), svc.bus, cfg, nil,
	)
}

// Project agent files must affect the tool schemas actually offered to each session.
type gatingHarness struct {
	*harness

	schemas *schemaRecorder
}

// schemaRecorder collects, per session, the union of tool names offered to the model across every provider call.
type schemaRecorder struct {
	mu    sync.Mutex
	names map[int64]map[string]bool
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

func newGatingHarness(
	t *testing.T,
	agents map[string]string,
	respond func(string, []llmwire.Message) *llmwire.Response,
) *gatingHarness {
	t.Helper()
	rec := &schemaRecorder{names: make(map[int64]map[string]bool)}
	h := newHarness(
		t,
		harnessOptions{
			configure: func(cfg *config.Config) { writeProjectAgents(t, cfg.WorkDir, agents) },
			clientFor: func(*config.Config) (llm.Client, error) {
				return &recordingLLM{respond: respond, rec: rec}, nil
			},
		},
	)
	h.mgr.applier = configapply.New(newTestConfigOps(t, t.TempDir()))
	return &gatingHarness{harness: h, schemas: rec}
}

// Root config tools require a real mutation layer backed by temporary files.
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

// contextInfo snapshots the seam observation the raw client can make: what the runner actually handed the session loop.
type contextInfo struct {
	hasDeadline bool
}

type scheduleRestartHarness struct {
	*harness
	db        *sql.DB
	closeOnce sync.Once
	closeErr  error
}

// harness wires a real sessionbuild.BuildInput (fake LLM) + daemon svc over a temp SQLite DB.
type harness struct {
	t         *testing.T
	db        *sql.DB
	mgr       *Service
	store     *sessionstore.Store
	links     subagent.Store
	schedules schedule.Store
	projectID int64
	ctx       context.Context
	opts      harnessOptions

	llmMu        sync.Mutex
	llmRefs      []*scriptedLLM // every client the session factory created
	wakeOnce     sync.Once
	shutdownOnce sync.Once
}

func (h *harness) startInboxWake() {
	h.wakeOnce.Do(func() { h.mgr.startWake() })
}

func (h *harness) restart(respond func(string, []llmwire.Message) *llmwire.Response) *harness {
	h.t.Helper()
	h.shutdown()
	o := h.opts
	if respond != nil {
		o.respond = respond
	}
	return newHarness(h.t, o)
}

func (h *harness) resume() {
	h.t.Helper()
	h.startInboxWake()
	h.mgr.resumeAfterRestart(h.ctx)
}

func (h *harness) send(prompt string, attrs map[string]any) int64 {
	h.t.Helper()
	h.startInboxWake()
	id, err := h.mgr.Send(h.ctx, h.projectID, prompt, "fake-model", attrs)
	require.NoError(h.t, err)
	return id
}

func (h *harness) say(id int64, prompt string) {
	h.t.Helper()
	h.startInboxWake()
	require.NoError(h.t, h.mgr.sendToSession(h.ctx, id, prompt))
}

func (h *harness) waitAnswer(id int64, answer string) {
	h.t.Helper()
	if answer != "" {
		h.waitUntil(answer, func() bool { return lastAssistantTextDTO(h.messages(id)) == answer })
	}
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(id) })
}

func (h *harness) ask(prompt, answer string) int64 {
	h.t.Helper()
	id := h.send(prompt, nil)
	h.waitAnswer(id, answer)
	return id
}

func (h *harness) reply(id int64, prompt, answer string) {
	h.t.Helper()
	h.say(id, prompt)
	h.waitAnswer(id, answer)
}

func (h *harness) waitDelivered(childID int64) {
	h.t.Helper()
	h.waitUntil("child delivery", func() bool {
		link := h.link(childID)
		return link != nil && link.DeliveredAt != 0
	})
}

func (h *harness) waitLink(parentID int64, callID string) subagent.Link {
	h.t.Helper()
	h.waitUntil("child link", func() bool { return h.linkByCall(parentID, callID) != nil })
	return *h.linkByCall(parentID, callID)
}

func (h *harness) collect() *eventCollector {
	h.t.Helper()
	c := collectEvents(h.t, h.mgr.bus.SubscribeAll())
	h.t.Cleanup(c.stop)
	return c
}

func (h *harness) waitProcess(id string, state backgroundprocess.State) backgroundprocess.Process {
	h.t.Helper()
	var record backgroundprocess.Process
	h.waitUntil("process state", func() bool {
		var err error
		record, err = h.mgr.processStore.GetProcess(context.Background(), id)
		return err == nil && record.State == state
	})
	return record
}

const taskCallID = "task-call-1"

// The child settles a tool round so compaction has an older raw group beyond its mandatory verbatim tail.
func subagentRespond(_ string, msgs []llmwire.Message) *llmwire.Response {
	if isCompactionInstruction(msgs) {
		return textReply("child checkpoint: ran ls, finished 42")
	}
	if hasUserContaining(msgs, "CHILD_TASK") {
		if hasToolResultFor(msgs, "ls") {
			return textReply("child finished: 42")
		}
		return callReply("child-ls-1", "ls", `{"path":"."}`)
	}
	if hasToolResultFor(msgs, "task") || hasUserContaining(msgs, "<subagent_completion>") {
		return textReply("all set, child launched")
	}
	return callReply(
		taskCallID, "task",
		`{"prompt":"CHILD_TASK do the thing","description":"child work","subagent_type":"general","background":true}`,
	)
}

type scriptCall struct{ id, name, params string }

type scriptTurn struct {
	marker string
	calls  []scriptCall
	answer string
}

type scriptTurns []scriptTurn

type harnessOptions struct {
	dbPath         string
	respond        func(string, []llmwire.Message) *llmwire.Response
	clientFor      func(*config.Config) (llm.Client, error)
	configure      func(*config.Config)
	links          func(subagent.Store) subagent.Store
	schedules      func(schedule.Store) schedule.Store
	observeRequest func(providerRequest)
}

func newHarness(t *testing.T, o harnessOptions) *harness {
	t.Helper()
	if o.dbPath == "" {
		o.dbPath = filepath.Join(t.TempDir(), "test.db")
	}
	ctx := context.Background()
	db, err := migrate.OpenDB(ctx, o.dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	provider, err := migrate.NewProvider(db)
	require.NoError(t, err)
	_, err = provider.Up(ctx)
	require.NoError(t, err)
	store := sessionstore.NewStore(db)
	links := subagent.NewStore(db, store)
	if o.links != nil {
		links = o.links(links)
	}
	schedules := schedule.NewStore(db, store)
	if o.schedules != nil {
		schedules = o.schedules(schedules)
	}
	cfg := &config.Config{WorkDir: t.TempDir(), Model: "fake-model"}
	if o.configure != nil {
		o.configure(cfg)
	}
	h := &harness{t: t, ctx: ctx, db: db, store: store, links: links, schedules: schedules, opts: o}
	h.projectID, err = store.GetOrCreateProject(ctx, cfg.WorkDir)
	require.NoError(t, err)
	if o.respond == nil {
		o.respond = subagentRespond
	}
	clientFor := o.clientFor
	if clientFor == nil {
		clientFor = func(*config.Config) (llm.Client, error) {
			client := &scriptedLLM{respond: o.respond}
			h.llmMu.Lock()
			h.llmRefs = append(h.llmRefs, client)
			h.llmMu.Unlock()
			return client, nil
		}
	}
	registry := mcpstore.NewStore(db)
	in := scriptedBuildInput(t, cfg, store, registry, clientFor, o.observeRequest)
	budgets := budget.New(store)
	scheduler := schedule.NewService(schedules, store)
	bus := sessionbus.New()
	applier := configapply.New(
		configops.New(filepath.Join(cfg.WorkDir, "config.yaml"), filepath.Join(cfg.WorkDir, "secrets")),
	)
	h.mgr = New(
		ctx, in, store, links, budgets, backgroundprocess.NewStore(db, store),
		progressruntime.New(store), bus, scheduler, cfg, registry, applier,
	)
	t.Cleanup(h.shutdown)
	return h
}

func countToolResultsFor(msgs []llmwire.Message, toolName string) int {
	count := 0
	for _, m := range msgs {
		if m.Role == llmwire.RoleTool && m.ToolName == toolName {
			count++
		}
	}
	return count
}

func countAssistantToolCallsFor(msgs []llmwire.Message, toolName string) int {
	count := 0
	for _, message := range msgs {
		if message.Role != llmwire.RoleAssistant {
			continue
		}
		for _, call := range message.ToolCalls {
			if call.Name == toolName {
				count++
			}
		}
	}
	return count
}

func (h *harness) shutdown() { h.shutdownOnce.Do(func() { h.mgr.Shutdown(h.ctx, 5*time.Second) }) }

func (h *harness) messages(parentID int64) []llmwire.Message {
	h.t.Helper()
	stored, err := h.store.LoadActiveMessages(h.ctx, parentID)
	require.NoError(h.t, err)
	return toDTO(stored)
}

func toDTO(stored []*transcript.Message) []llmwire.Message {
	msgs := make([]llmwire.Message, 0, len(stored))
	for _, m := range stored {
		msg := llmwire.Message{
			Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID, ToolName: m.ToolName,
			ToolError: m.ToolError,
		}
		if len(m.ToolCalls) > 0 {
			var tcs []llmwire.ToolCall
			if err := json.Unmarshal(m.ToolCalls, &tcs); err == nil {
				msg.ToolCalls = tcs
			}
		}
		msgs = append(msgs, msg)
	}
	return msgs
}

func hasUserContaining(msgs []llmwire.Message, needle string) bool {
	for _, m := range msgs {
		if m.Role == llmwire.RoleUser && strings.Contains(m.Content, needle) {
			return true
		}
	}
	return false
}

func hasToolResultFor(msgs []llmwire.Message, toolName string) bool {
	for _, m := range msgs {
		if m.Role == llmwire.RoleTool && m.ToolName == toolName {
			return true
		}
	}
	return false
}

func (h *harness) createChild(parentID int64, link subagent.Link) int64 {
	h.t.Helper()
	return h.createChildSession(parentID, "fake-model", &link)
}

func (h *harness) createUnlinkedChild(parentID int64) int64 {
	h.t.Helper()
	return h.createChildSession(parentID, "fake-model", nil)
}

func (h *harness) createChildSession(parentID int64, model string, link *subagent.Link) int64 {
	h.t.Helper()
	parent, err := h.store.GetSession(h.ctx, parentID)
	require.NoError(h.t, err)
	require.NotNil(h.t, parent)
	rootID := parent.RootID
	if rootID == 0 {
		rootID = parentID
	}
	var childID int64
	require.NoError(h.t, h.store.WithTx(h.ctx, func(tx *sql.Tx) error {
		var createErr error
		childID, createErr = sessionstore.CreateSubagentSessionTx(h.ctx, tx, sessionstore.CreateSubagentSession{
			ProjectID: parent.ProjectID, ParentID: parentID, RootID: rootID,
			AgentType: "general", Model: model, ReasoningLevel: "",
		})
		if createErr != nil || link == nil {
			return createErr
		}
		link.ParentID, link.ChildID = parentID, childID
		return insertChildLink(h.ctx, tx, *link)
	}))
	return childID
}

func (h *harness) attachChildLink(link subagent.Link) {
	h.t.Helper()
	require.NoError(h.t, h.store.WithTx(h.ctx, func(tx *sql.Tx) error {
		return insertChildLink(h.ctx, tx, link)
	}))
}

func insertChildLink(ctx context.Context, tx *sql.Tx, link subagent.Link) error {
	if link.State == "" {
		link.State = subagent.StateSpawned
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO subagent_links
		(parent_id,child_id,task_call_id,blocking,depth,state,created_at,result,outcome) VALUES (?,?,?,?,?,?,?,?,?)`,
		link.ParentID, link.ChildID, link.TaskCallID, link.Blocking, link.Depth, link.State,
		time.Now().UTC().Unix(), link.Result, link.Outcome)
	return err
}

func seedTerminalChild(ctx context.Context, sessions *sessionstore.Store, childID int64,
	state subagent.State, result string, outcome subagent.Outcome,
) error {
	return sessions.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE subagent_links SET state=?,result=?,outcome=? WHERE child_id=?`,
			state, result, outcome, childID)
		return err
	})
}

func finalizeTestChild(ctx context.Context, t *testing.T, manager *Service, childID int64) {
	t.Helper()
	unlock, err := manager.lockSessionTree(ctx, childID)
	require.NoError(t, err)
	deliver := manager.finalizeChildLocked(ctx, childID, false)
	unlock()
	if deliver != nil {
		deliver()
	}
}

func waitForScenarioSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal(description + " did not start")
	}
}

func TestMain(m *testing.M) {
	if handled, err := backgroundprocess.RunGuardian(os.Args[1:]); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	home, err := os.MkdirTemp("", "daemon-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.Setenv("HOME", home); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}

type mockSession struct {
	*scriptedLLM
	mu            sync.Mutex
	ran           bool
	runErr        error
	completeAfter time.Duration
}

type mockFactory struct {
	mu       sync.Mutex
	sessions []*mockSession
	nextSess *mockSession
}

func (m *mockSession) Chat(
	ctx context.Context,
	_ string,
	_ []llmwire.Message,
	_ []llmwire.ToolSchema,
	_ ...llmwire.ChatOption,
) (*llmwire.Response, error) {
	m.mu.Lock()
	m.ran = true
	delay, err := m.completeAfter, m.runErr
	m.mu.Unlock()
	if delay <= 0 {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(delay):
	}
	if err != nil {
		return nil, err
	}
	return &llmwire.Response{Text: "done", FinishType: llmwire.FinishStop}, nil
}

func (f *mockFactory) client(*config.Config) (llm.Client, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	client := f.nextSess
	f.nextSess = nil
	if client == nil {
		client = &mockSession{}
	}
	client.scriptedLLM = &scriptedLLM{}
	f.sessions = append(f.sessions, client)
	return client, nil
}

func withTestModels(cfg *config.Config) {
	cfg.UnifiedConfig = &config.UnifiedConfig{
		Models: []config.ModelEntry{
			{ID: "fake-model"},
			{ID: "test-model"},
			{ID: "my-model"},
			{ID: "old-model"},
			{ID: "new-model"},
		},
	}
	for index := range cfg.UnifiedConfig.Models {
		cfg.UnifiedConfig.Models[index].Reasoning = &config.ReasoningSpec{
			Supported:    true,
			NativeEffort: true,
			Efforts:      []string{"low", "medium", "high"},
		}
		cfg.UnifiedConfig.Models[index].EffortLevels = []string{"low", "medium", "high"}
	}
}

// Spawn and call logs expose ownership of each session's MCP stack.
const fakeMCPScript = `#!/bin/sh
LOG="$1"
PONG="$2"
RELEASE="$3"
(
  parent=$$
  while kill -0 "$parent" 2>/dev/null; do sleep 0.01; done
  echo exit >> "$LOG"
) &
echo spawn >> "$LOG"
echo $$ > "$LOG.pid"
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  [ -n "$id" ] || continue
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2024-11-05","capabilities":{"tools":{}},"serverInfo":{"name":"fakemcp","version":"0.0.1"}}}\n' "$id"
      ;;
    *'"method":"tools/list"'*)
      if [ -e "$LOG.extra" ]; then
        tools='[{"name":"ping","description":"Answers pong.","inputSchema":{"type":"object","properties":{}}},{"name":"ping2","description":"Newly available tool.","inputSchema":{"type":"object","properties":{}}}]'
      else
        tools='[{"name":"ping","description":"Answers pong.","inputSchema":{"type":"object","properties":{}}}]'
      fi
      printf '{"jsonrpc":"2.0","id":%s,"result":{"tools":%s}}\n' "$id" "$tools"
      ;;
    *'"method":"tools/call"'*)
      if [ -p "$RELEASE" ]; then
        exec 3<> "$RELEASE"
        echo call >> "$LOG"
        read -r released <&3 || exit 1
      else
        echo call >> "$LOG"
      fi
      printf '{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"%s pid=%s"}]}}\n' "$id" "$PONG" "$$"
      ;;
    *)
      printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"Method not found"}}\n' "$id"
      ;;
  esac
done
`

func mcpPingCall(id string) *llmwire.Response {
	return callReply(id, "mcp__fake__ping", `{}`)
}

func toolResultForCallID(msgs []llmwire.Message, callID string) *llmwire.Message {
	for _, m := range msgs {
		if m.Role == llmwire.RoleTool && m.ToolCallID == callID {
			return &m
		}
	}
	return nil
}

// Shared trace ownership matches the manager used by output-chain scenarios.
const scenarioManagerID = "telegram:main"

func installScenarioProcessService(t *testing.T, h *harness) backgroundprocess.Service {
	t.Helper()
	service := backgroundprocess.NewService(h.mgr.processStore, backgroundprocess.Options{OutputDir: t.TempDir()})
	h.mgr.processes = service
	h.mgr.build.ProcessService = service
	h.startInboxWake()
	return service
}

func startScenarioProcess(
	t *testing.T,
	service backgroundprocess.Service,
	ownerID, rootID int64,
	command string,
) backgroundprocess.Process {
	t.Helper()
	record, err := service.Start(context.Background(), backgroundprocess.Spec{
		ProjectDir: "project-test", SessionID: ownerID, RootSessionID: rootID, ToolCallID: "process-call",
		Deadline: 30 * time.Second, Advertise: true,
	}, func(ctx context.Context) (*exec.Cmd, error) {
		return exec.CommandContext(ctx, "sh", "-c", command), nil
	})
	require.NoError(t, err)
	return record
}

// Collect the full notification stream before asserting its order to avoid subscriber races.
type eventCollector struct {
	t       *testing.T
	mu      sync.Mutex
	events  []controllerapi.SessionNotification
	done    chan struct{}
	changed chan struct{}
}

func countPublishedMessage(events []controllerapi.SessionNotification, sessionID int64, message string) int {
	count := 0
	for _, event := range events {
		if event.SessionID == sessionID && event.Notification.Type == sessionevent.NotifyMessage &&
			event.Notification.Message == message {
			count++
		}
	}
	return count
}

func hasStateEvent(events []controllerapi.SessionNotification, sessionID int64, state controllerapi.State) bool {
	for _, event := range events {
		if event.SessionID == sessionID && event.Notification.Type == sessionevent.NotifyStateChanged &&
			event.Notification.Status == state {
			return true
		}
	}
	return false
}

func collectEvents(t *testing.T, ch <-chan controllerapi.SessionNotification) *eventCollector {
	t.Helper()
	c := &eventCollector{t: t, done: make(chan struct{}), changed: make(chan struct{}, 1)}
	go func() {
		for {
			select {
			case sn, ok := <-ch:
				if !ok {
					return
				}

				c.mu.Lock()
				c.events = append(c.events, sn)
				c.mu.Unlock()

				select {
				case c.changed <- struct{}{}:
				default:
				}
			case <-c.done:
				return
			}
		}
	}()
	return c
}

func (c *eventCollector) snapshot() []controllerapi.SessionNotification {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]controllerapi.SessionNotification, len(c.events))
	copy(out, c.events)
	return out
}

func (c *eventCollector) waitMessage(sessionID int64, message string) {
	c.t.Helper()
	c.waitFor(c.t, message, func(events []controllerapi.SessionNotification) bool {
		return countPublishedMessage(events, sessionID, message) == 1
	})
}

func (c *eventCollector) waitState(sessionID int64, state controllerapi.State) {
	c.t.Helper()
	c.waitFor(c.t, "session state", func(events []controllerapi.SessionNotification) bool {
		return hasStateEvent(events, sessionID, state)
	})
}

// waitIdleAfter returns the trace through the turn's own idle; a late wake may re-activate the session after it.
func (c *eventCollector) waitIdleAfter(sessionID int64, message string) []controllerapi.SessionNotification {
	c.t.Helper()
	var turn []controllerapi.SessionNotification
	c.waitFor(c.t, "idle after "+message, func(events []controllerapi.SessionNotification) bool {
		messageSeen := false
		for i, event := range events {
			if event.SessionID != sessionID {
				continue
			}
			if event.Notification.Type == sessionevent.NotifyMessage && event.Notification.Message == message {
				messageSeen = true
			}
			if messageSeen && event.Notification.Type == sessionevent.NotifyStateChanged &&
				event.Notification.Status == sessionevent.StateIdle {
				turn = events[:i+1]
				return true
			}
		}
		return false
	})
	return turn
}

func (c *eventCollector) waitWait(sessionID int64, kind sessionevent.WaitKind) {
	c.t.Helper()
	c.waitFor(c.t, fmt.Sprintf("waiting kind %s", kind), func(events []controllerapi.SessionNotification) bool {
		for _, event := range events {
			if event.SessionID != sessionID || event.Notification.Type != sessionevent.NotifyWaiting {
				continue
			}
			for _, item := range event.Notification.Waiting {
				if item.Kind == kind {
					return true
				}
			}
		}
		return false
	})
}

func (c *eventCollector) waitFor(
	t *testing.T,
	label string,
	condition func([]controllerapi.SessionNotification) bool,
) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		if condition(c.snapshot()) {
			return
		}
		select {
		case <-c.changed:
		case <-timer.C:
			t.Fatalf("timed out waiting for controller trace: %s; events: %+v", label, c.snapshot())
		}
	}
}

func (c *eventCollector) stop() { close(c.done) }

func requireNotification(t *testing.T, ch <-chan controllerapi.SessionNotification) controllerapi.SessionNotification {
	t.Helper()
	select {
	case sn := <-ch:
		return sn
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a notification")
	}
	return controllerapi.SessionNotification{}
}

func requireNoNotification(t *testing.T, ch <-chan controllerapi.SessionNotification) {
	t.Helper()
	select {
	case sn := <-ch:
		t.Fatalf("unexpected notification for session %d: %+v", sn.SessionID, sn.Notification)
	case <-time.After(200 * time.Millisecond):
	}
}

func scenarioTranscriptText(messages []llmwire.Message) string {
	var text strings.Builder
	for _, message := range messages {
		text.WriteString(message.Content)
		for _, call := range message.ToolCalls {
			text.Write(call.Arguments)
		}
	}
	return text.String()
}

func runnerWaitingCount(runners *runnerSet) int {
	runners.mu.Lock()
	defer runners.mu.Unlock()
	return len(runners.waiting)
}

func runnerLiveCount(runners *runnerSet) int {
	runners.mu.Lock()
	defer runners.mu.Unlock()
	return len(runners.byID)
}

func runnerRunningCount(runners *runnerSet) int {
	runners.mu.Lock()
	defer runners.mu.Unlock()
	return runners.running
}

func runnerChildCount(runners *runnerSet) int {
	runners.mu.Lock()
	defer runners.mu.Unlock()
	return runners.children
}

func requireNoManagerNotification(t *testing.T, ch <-chan controllerapi.SessionNotification) {
	t.Helper()
	select {
	case notification := <-ch:
		t.Fatalf("unexpected manager notification: %#v", notification)
	case <-time.After(20 * time.Millisecond):
	}
}

func configuredClient(configure func(*config.Config)) func(*config.Config) (llm.Client, error) {
	cfg := &config.Config{}
	configure(cfg)
	return func(view *config.Config) (llm.Client, error) {
		return llm.NewClientWithModel(cfg, view.Model)
	}
}

func withKnownModels(known []string) func(*config.Config) {
	return func(cfg *config.Config) {
		cfg.Model = known[0]
		cfg.UnifiedConfig = &config.UnifiedConfig{}
		for _, model := range known {
			cfg.UnifiedConfig.Models = append(
				cfg.UnifiedConfig.Models, config.ModelEntry{ID: model, ContextWindow: 200000},
			)
		}
	}
}

func knownModelClient(
	known []string,
	respond func(string, []llmwire.Message) *llmwire.Response,
) func(*config.Config) (llm.Client, error) {
	return func(c *config.Config) (llm.Client, error) {
		if !slices.Contains(known, c.Model) {
			return nil, fmt.Errorf("model %q not found in config", c.Model)
		}
		return &scriptedLLM{respond: respond}, nil
	}
}

type skillHarness struct {
	*harness
	recorder *skillRecorder
}

type skillRecorder struct {
	mu    sync.Mutex
	calls []skillCall
}

type skillCall struct {
	system string
	msgs   []llmwire.Message
}

func (r *skillRecorder) wrap(
	respond func(string, []llmwire.Message) *llmwire.Response,
) func(string, []llmwire.Message) *llmwire.Response {
	return func(system string, msgs []llmwire.Message) *llmwire.Response {
		r.mu.Lock()
		r.calls = append(r.calls, skillCall{system: system, msgs: append([]llmwire.Message(nil), msgs...)})
		r.mu.Unlock()
		return respond(system, msgs)
	}
}

func (r *skillRecorder) snapshot() []skillCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]skillCall(nil), r.calls...)
}

func (h *harness) workDir() string {
	h.t.Helper()
	workDir, err := h.mgr.store.GetProjectWorkDir(h.ctx, h.projectID)
	require.NoError(h.t, err)
	return workDir
}

// plainRespond answers every turn with text, so a session reaches idle in one turn.
func plainRespond(_ string, _ []llmwire.Message) *llmwire.Response {
	return textReply("work done")
}

func (h *harness) requireInboxDrained(sessionID int64) {
	h.t.Helper()
	_, err := h.store.PeekPending(h.ctx, sessionID)
	require.ErrorIs(h.t, err, sessionstore.ErrNoPendingInput, "the rejected input must leave the inbox")
}

func withSpawnEffortModels(baseURL string) func(*config.Config) {
	return func(cfg *config.Config) {
		cfg.Model = "parent-model"
		cfg.UnifiedConfig = &config.UnifiedConfig{
			Providers: map[string]config.ProviderEntry{"or": {Driver: "openrouter", APIKey: "key", BaseURL: baseURL}},
			Models: []config.ModelEntry{
				reasoningModelEntry("parent-model", []string{"low", "high"}, "high"),
				reasoningModelEntry("child-model", []string{"low", "medium"}, "low"),
			},
		}
	}
}

func reasoningModelEntry(id string, efforts []string, defaultEffort string) config.ModelEntry {
	return config.ModelEntry{
		ID: id, Provider: "or", ContextWindow: 200_000, MaxTokens: 64_000,
		EffortLevels:  efforts,
		DefaultEffort: defaultEffort,
		Reasoning:     &config.ReasoningSpec{Supported: true, Efforts: efforts},
	}
}

func requireBarrierSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal(message)
	}
}

func testProject(t *testing.T, s interface {
	GetOrCreateProject(context.Context, string) (int64, error)
}, workDir string,
) int64 {
	t.Helper()
	pid, err := s.GetOrCreateProject(context.Background(), workDir)
	require.NoError(t, err)
	return pid
}

func createBackgroundChild(t *testing.T, mgr *Service, projectID, parentID int64) int64 {
	t.Helper()
	parent, err := mgr.store.GetSession(context.Background(), parentID)
	require.NoError(t, err)
	rootID := parent.RootID
	if rootID == 0 {
		rootID = parentID
	}
	childID, err := mgr.links.Create(context.Background(), subagent.Create{
		ProjectID:  projectID,
		ParentID:   parentID,
		RootID:     rootID,
		Model:      "fake-model",
		TaskCallID: "task-follow-up",
		State:      subagent.StateSpawned,
	})
	require.NoError(t, err)
	return childID
}

func containsAll(value string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(value, needle) {
			return false
		}
	}
	return true
}

type outboxRow struct {
	ID        int64
	SessionID int64
	Type      string
	SourceKey string
	Content   string
}

func managerAttrs(id string) map[string]any {
	return map[string]any{"manager_id": id}
}

func textReply(text string) *llmwire.Response {
	return &llmwire.Response{Text: text}
}

func callReply(id, name, args string) *llmwire.Response {
	return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{ID: id, Name: name, Arguments: []byte(args)}}}
}

func (h *harness) session(id int64) *sessionstore.SessionRecord {
	h.t.Helper()
	record, err := h.store.GetSession(h.ctx, id)
	require.NoError(h.t, err)
	require.NotNil(h.t, record)
	return record
}

func (h *harness) link(childID int64) *subagent.Link {
	h.t.Helper()
	link, err := h.links.GetLink(h.ctx, childID)
	require.NoError(h.t, err)
	return link
}

func (h *harness) linkByCall(parentID int64, callID string) *subagent.Link {
	h.t.Helper()
	link, err := h.links.GetLinkByTaskCallID(h.ctx, parentID, callID)
	require.NoError(h.t, err)
	return link
}

func (h *harness) outbox(sessionID int64) []outboxRow {
	h.t.Helper()
	rows, err := h.db.QueryContext(
		h.ctx,
		`SELECT id, session_id, type, COALESCE(source_key, ''), content
		FROM session_outbox WHERE session_id = ? ORDER BY id`,
		sessionID,
	)
	require.NoError(h.t, err)
	defer rows.Close()
	var out []outboxRow
	for rows.Next() {
		var row outboxRow
		require.NoError(
			h.t, rows.Scan(&row.ID, &row.SessionID, &row.Type, &row.SourceKey, &row.Content),
		)
		out = append(out, row)
	}
	require.NoError(h.t, rows.Err())
	return out
}

func (h *harness) createRoot(attrs map[string]any) int64 {
	h.t.Helper()
	root, err := h.store.CreateSession(h.ctx, h.projectID, "fake-model", "", attrs)
	require.NoError(h.t, err)
	require.NotNil(h.t, root)
	return root.ID
}

func newMockHarness(t *testing.T) (*harness, *mockFactory, *eventCollector) {
	t.Helper()
	factory := &mockFactory{}
	h := newHarness(t, harnessOptions{configure: withTestModels, clientFor: factory.client})
	return h, factory, h.collect()
}

func newFlakyLinkHarness(
	t *testing.T,
	respond func(string, []llmwire.Message) *llmwire.Response,
) (*harness, *flakyLinkStore) {
	t.Helper()
	var flaky *flakyLinkStore
	h := newHarness(t, harnessOptions{respond: respond, links: func(inner subagent.Store) subagent.Store {
		flaky = newFlakyLinkStore(inner)
		return flaky
	}})
	return h, flaky
}

func (h *harness) acceptUserInput(sessionID int64, raw, accepted string) int64 {
	h.t.Helper()
	input, err := h.store.Enqueue(
		h.ctx, sessionstore.Input{SessionID: sessionID, Source: sessionstore.InputSourceUser, Content: raw},
	)
	require.NoError(h.t, err)
	_, err = h.store.Commit(h.ctx, sessionstore.Commit{
		SessionID: input.Input.SessionID,
		Accept: []sessionstore.Accept{{
			InputID: input.Input.ID, State: sessionstore.InputStateAccepted,
			Content: accepted, LinkRef: -1, ModelBound: true,
		}},
	})
	require.NoError(h.t, err)
	return input.Input.ID
}

func (h *harness) addOneShot(sessionID int64, due time.Time, content string, fresh bool) *schedule.Schedule {
	h.t.Helper()
	entry, err := h.schedules.AddSchedule(h.ctx, sessionID, "", &due, content, fresh, schedule.ScheduleMetadata{})
	require.NoError(h.t, err)
	return entry
}

func (h *harness) countOutbox(sessionID int64, match func(string) bool) int {
	h.t.Helper()
	count := 0
	for _, row := range h.outbox(sessionID) {
		if match(row.Content) {
			count++
		}
	}
	return count
}

// closedInTime keeps a broken flow from stranding the test or the session's goroutine.
func closedInTime(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	case <-time.After(5 * time.Second):
		return false
	}
}

func newScheduleRestartHarness(
	t *testing.T,
	dbPath, workDir string,
	respond func(string, []llmwire.Message) *llmwire.Response,
) *scheduleRestartHarness {
	t.Helper()
	base := newHarness(
		t,
		harnessOptions{dbPath: dbPath, respond: respond, configure: func(cfg *config.Config) { cfg.WorkDir = workDir }},
	)
	h := &scheduleRestartHarness{harness: base, db: base.db}
	t.Cleanup(func() { require.NoError(t, h.close()) })
	return h
}

func (h *scheduleRestartHarness) close() error {
	h.closeOnce.Do(func() {
		h.shutdown()
		h.closeErr = h.db.Close()
	})
	return h.closeErr
}

func (s scriptTurns) respond(_ string, msgs []llmwire.Message) *llmwire.Response {
	last := lastUserText(msgs)
	for _, turn := range s {
		if !strings.Contains(last, turn.marker) {
			continue
		}
		for _, call := range turn.calls {
			if toolResultForCallID(msgs, call.id) == nil {
				return callReply(call.id, call.name, call.params)
			}
		}
		return textReply(turn.answer)
	}
	return textReply("unscripted")
}

func newBrowserMCPServer(t *testing.T) (string, string) {
	t.Helper()
	encoded := base64.StdEncoding.EncodeToString(browserScenarioPNG(t))
	dir := t.TempDir()
	server := filepath.Join(dir, "browser.sh")
	log := filepath.Join(dir, "calls.log")
	require.NoError(
		t,
		os.WriteFile(server, []byte(strings.ReplaceAll(browserScenarioScript, "IMAGE_DATA", encoded)), 0o700),
	)
	require.NoError(t, os.WriteFile(log, nil, 0o600))
	return server, log
}

func browserFrameRespond(capture func([]llmwire.Message)) func(string, []llmwire.Message) *llmwire.Response {
	return func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "GENERAL_CHILD") {
			return textReply("general complete")
		}
		if hasUserContaining(msgs, "BROWSER_CHILD") {
			if capture != nil {
				capture(msgs)
			}
			for _, step := range []struct{ id, args string }{
				{"frame-1", `{"task_state":"open page"}`},
				{"missing", `{}`},
				{"frame-2", `{"task_state":"first frame showed one item; inspect next"}`},
				{"server-error", `{"task_state":"second frame showed two items; retry after server failure","fail":true}`},
				{"frame-3", `{"task_state":"second frame showed two items; finish"}`},
			} {
				if toolResultForCallID(msgs, step.id) == nil {
					return callReply(step.id, "mcp__remote__"+"view", step.args)
				}
			}
			return textReply("browser complete")
		}
		if toolResultForCallID(msgs, "general-task") != nil {
			return textReply("root complete")
		}
		if toolResultForCallID(msgs, "browser-task") != nil {
			return callReply(
				"general-task",
				tool.IDTask,
				`{"prompt":"GENERAL_CHILD inspect tools","description":"inspect tools","subagent_type":"general"}`,
			)
		}
		return callReply(
			"browser-task",
			tool.IDTask,
			`{"prompt":"BROWSER_CHILD inspect example.test and report facts","description":"inspect site","subagent_type":"browser"}`,
		)
	}
}

func parkOnBlockingChild(t *testing.T, h *harness) (int64, subagent.Link) {
	t.Helper()
	parentID := h.send("do work then spawn", nil)
	link := h.waitLink(parentID, taskCallID)
	require.True(t, link.Blocking)
	h.waitUntil("parent suspended", func() bool { return !h.mgr.HasActiveLoop(parentID) })
	return parentID, link
}

func browserFrameOptions(dbPath, workDir string,
	respond func(string, []llmwire.Message) *llmwire.Response,
	observe func(providerRequest),
) harnessOptions {
	return harnessOptions{dbPath: dbPath, respond: respond, configure: func(cfg *config.Config) {
		cfg.WorkDir = workDir
		cfg.UnifiedConfig = &config.UnifiedConfig{
			Browser: &config.BrowserConfig{MCPName: "remote"},
			Models: []config.ModelEntry{
				{ID: "fake-model", ContextWindow: 200000, InputModalities: []string{"text", "image"}},
			},
		}
	}, observeRequest: observe}
}

func childRequestAfter(requests []providerRequest, childID int64, after int) (providerRequest, bool) {
	for _, request := range requests[after:] {
		if strings.HasSuffix(request.SessionID, ":"+strconv.FormatInt(childID, 10)) {
			return request, true
		}
	}
	return providerRequest{}, false
}

func lastToolResultContent(msgs []llmwire.Message, toolName string) string {
	for _, v := range slices.Backward(msgs) {
		if v.Role == llmwire.RoleTool && v.ToolName == toolName {
			return v.Content
		}
	}
	return ""
}

func backgroundProcessID(messages []llmwire.Message) string {
	for _, message := range slices.Backward(messages) {
		if message.Role != llmwire.RoleTool || message.ToolName != "bash" {
			continue
		}
		_, after, found := strings.Cut(message.Content, backgroundProcessPrefix)
		if !found {
			return ""
		}
		id, _, _ := strings.Cut(after, "\n")
		return id
	}
	return ""
}

func countSubagentCompletions(messages []llmwire.Message, childID int64) int {
	needle := "child_id: " + strconv.FormatInt(childID, 10)
	count := 0
	for _, message := range messages {
		if message.Role == llmwire.RoleUser && strings.Contains(message.Content, "<subagent_completion>") &&
			strings.Contains(message.Content, needle) {
			count++
		}
	}
	return count
}

func countMessageContentContaining(messages []llmwire.Message, fragment string) int {
	count := 0
	for _, message := range messages {
		if strings.Contains(message.Content, fragment) {
			count++
		}
	}
	return count
}

func countUserCompletions(messages []llmwire.Message, marker string) int {
	count := 0
	for _, message := range messages {
		if message.Role == llmwire.RoleUser && strings.Contains(message.Content, marker) {
			count++
		}
	}
	return count
}

func (h *harness) waitParentAnswer(parentID int64) {
	h.t.Helper()
	h.waitUntil("parent answer", func() bool {
		return lastAssistantTextDTO(h.messages(parentID)) == "parent final" && !h.mgr.HasActiveLoop(parentID)
	})
}

func (h *harness) processInputs(processID string) int {
	h.t.Helper()
	var count int
	require.NoError(h.t, h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM session_inbox
		WHERE source = 'process' AND json_extract(attributes, '$.process_id') = ?`, processID).Scan(&count))
	return count
}

func newSkillHarness(
	t *testing.T,
	skills map[string]string,
	respond func(string, []llmwire.Message) *llmwire.Response,
) *skillHarness {
	t.Helper()
	rec := &skillRecorder{}
	h := newHarness(t, harnessOptions{respond: rec.wrap(respond)})
	for name, body := range skills {
		dir := filepath.Join(h.workDir(), ".claude", "skills", name)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o600))
	}
	return &skillHarness{harness: h, recorder: rec}
}

func skillDoc(name, description, body string, extraFrontmatter ...string) string {
	var b strings.Builder
	b.WriteString("---\nname: " + name + "\ndescription: " + description + "\n")
	for _, line := range extraFrontmatter {
		b.WriteString(line + "\n")
	}
	b.WriteString("---\n" + body + "\n")
	return b.String()
}

func readHarnessTraceFile(t *testing.T, path string) harnessTraceFile {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err, "missing recorded trace; regenerate with -update-traces")
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var file harnessTraceFile
	require.NoError(t, decoder.Decode(&file))
	require.NotEmpty(t, file.Trace)
	return file
}

func lastUserText(msgs []llmwire.Message) string {
	const nudgePrefix = "You ended your previous response without calling a tool."
	text := ""
	for _, m := range msgs {
		// Confirmation must answer the task again; its host nudge is not a new user registration turn.
		if m.Role == llmwire.RoleUser && !strings.HasPrefix(m.Content, nudgePrefix) {
			text = m.Content
		}
	}
	return text
}

func sleepThenAnswer(answer string) func(string, []llmwire.Message) *llmwire.Response {
	return func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasToolResultFor(messages, tool.IDSleep) {
			return textReply(answer)
		}
		return callReply("scenario-sleep", tool.IDSleep, `{"duration":"1h","reason":"scenario"}`)
	}
}

func (r *skillRecorder) assertAllPaired(t *testing.T) {
	t.Helper()
	for i, req := range r.snapshot() {
		assert.NoErrorf(
			t,
			llmtest.ValidateToolPairing(req.msgs),
			"request %d reached the provider with a dangling tool_use",
			i,
		)
	}
}
