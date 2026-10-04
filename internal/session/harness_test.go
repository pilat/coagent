package session

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionprompt"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/tool/builtin"
	"github.com/pilat/coagent/internal/transcript"
)

// validSummary is the scripted summarizer's canonical answer. The runtime never
// requires headings — this shape simply exercises a plausible completed text.
const validSummary = "Checkpoint: goal was to fix the auth bug; login.go edited; tests pass; next: run e2e."

const (
	cmdQueueCompact compactionCommand = iota
	cmdStartExternalCall
	cmdDeliverExternalResult
	cmdEmitToolCall
	cmdExecuteTools
	cmdSupersedeWithUserTurn
	cmdRunLoopPoint
)

const (
	testPrompt       = "test prompt"
	testReasoningLvl = "medium"
	testMockModel    = "mock"
)

// mapOrderTrials is the number of repetitions used to expose logic that depends
// on Go's randomized map iteration order. A single pass would catch it only by
// luck.
const mapOrderTrials = 50

// untrustedTestWindow mirrors scriptedLLM.ContextWindow (200k) so scenario and
// unit expectations stay aligned.
const untrustedTestWindow = 200000

var demoRefs = []llmwire.ImageRef{
	{
		Path: "/project/coagent-a.png", ReadRoot: "/project", ReadRootID: "1a:2b",
		Mime: llmwire.MimeImagePng, Size: 4096,
	},
	{Path: "/tmp/coagent-b.jpg", Mime: llmwire.MimeImageJpeg, Size: 8192},
}

var compactionAlphabet = []compactionCommand{
	cmdQueueCompact,
	cmdStartExternalCall,
	cmdDeliverExternalResult,
	cmdEmitToolCall,
	cmdExecuteTools,
	cmdSupersedeWithUserTurn,
	cmdRunLoopPoint,
}

var errStoreDown = errors.New("store unavailable")

// reasoningBlob stands in for a provider's sealed reasoning payload. The session
// never opens it, so an opaque blob is exactly as good as a real envelope here.
var reasoningBlob = json.RawMessage(`{"opaque":"payload"}`)

type imageStubTool struct {
	id     string
	err    error
	result *tool.Result
}

// compactionCommand is deliberately smaller than the implementation API: these
// are the externally meaningful transitions whose interleaving decides whether
// compaction may run.
type compactionCommand byte

// compactionProtocolModel is the reference: what a reader of the plan expects,
// independent of how the session implements it. The verbatim tail is never
// empty (D3), so a compaction needs raw rows besides the newest legal group —
// one group to summarize, one to keep. rawRows counts raw rows since the last
// compaction; tailGroupRows is the newest legal group's size.
type compactionProtocolModel struct {
	queuedCompact    bool
	externalPending  bool
	workPending      bool
	freshContent     bool
	rawRows          int
	tailGroupRows    int
	compactionsRun   int
	compactionsHoped int
}

// compactionMockLLM is a mock implementation for compaction tests.
type compactionMockLLM struct {
	response      *llmwire.Response
	err           error
	callCount     int
	lastMessages  []llmwire.Message
	contextWindow int
	// chat, when set, drives a call sequence and sees the rendered prompt.
	chat        func(callIndex int, prompt string) (*llmwire.Response, error)
	prompts     []string
	lastOptions llmwire.ChatOptions
}

type compactionRecordingStore struct {
	mockSessionStore
	nextID           int64
	messages         []*transcript.Message
	positions        map[int64]int
	markCompactedErr error
	markCompacted    int
	insertFailAt     int // 1-based appendRow call index that returns insertErr
	insertCalls      int
	insertErr        error
	replaceErr       error
}

// countingTool records how many times it ran — the whole point of the staged
// mechanism is that an applied config change never runs twice.
type countingTool struct {
	id   string
	runs atomic.Int64
}

type mockSessionStore struct {
	sessionstore.Store
	nextMsgID                 int64
	messages                  []*transcript.Message
	insertCalls, insertFailAt int
	insertErr                 error
	completion                sessionstore.CompletionCheckState
}

type noteEvents struct{ notes *[]string }

type mockLLMRunOnce struct {
	response *llmwire.Response
	called   bool
}

type loopScriptLLM struct {
	mockLLMRunOnce
	responses    []*llmwire.Response
	calls        int
	err          error
	lastTools    []llmwire.ToolSchema
	lastMessages []llmwire.Message
	onCall       func(int, []llmwire.Message) (*llmwire.Response, error)
}

type nilResultTool struct{}

// stubTool is a minimal tool implementation for integration tests.
type stubTool struct {
	id            string
	result        string
	err           error
	parallelSafe  bool
	resultIsError bool
}

type trackedClient struct {
	mockLLMRunOnce
	closed    int
	sessionID string
}

type reasoningSwitchClient struct {
	mockLLMRunOnce
	requests [][]llmwire.Message
}

// reasoningLoadStore serves a fixed transcript so the load-time projection can be
// asserted without a real database.
type reasoningLoadStore struct {
	mockSessionStore

	messages []*transcript.Message
}

// statusStubStore returns fixed lifetime totals for buildSessionStatus tests.
type statusStubStore struct {
	mockSessionStore
	in, out   int
	cost      float64
	subagents int
}

// gateTool blocks the entry-numbered call until its release channel closes and
// records the entry order, so stage tests assert scheduling without racing
// goroutines. Call index rides in arguments as {"n":"..."}.
type gateTool struct {
	id           string
	parallelSafe bool

	mu       sync.Mutex
	releases map[int]chan struct{}
	entered  []string
	errFor   map[string]error
	typedFor map[string]bool
}

func (t *imageStubTool) ID() string { return t.id }

func (t *imageStubTool) ParallelSafe() bool { return false }

func (t *imageStubTool) Description() string { return "stub" }

func (t *imageStubTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

func (t *imageStubTool) Execute(context.Context, json.RawMessage) (*tool.Result, error) {
	if t.err != nil {
		return nil, t.err
	}

	return t.result, nil
}

func (s *compactionRecordingStore) Commit(
	ctx context.Context,
	c sessionstore.Commit,
) (*sessionstore.CommitResult, error) {
	beforeMessages := make([]*transcript.Message, len(s.messages))
	for i, row := range s.messages {
		copyRow := *row
		beforeMessages[i] = &copyRow
	}
	beforePositions := maps.Clone(s.positions)
	beforeID := s.nextID
	result := &sessionstore.CommitResult{}
	if c.Replace != nil {
		if _, err := s.replaceRows(ctx, c.SessionID, c.Replace.HeadIDs, c.Replace.Entries); err != nil {
			s.messages = beforeMessages
			s.positions = beforePositions
			s.nextID = beforeID
			return nil, err
		}
	}
	for _, row := range append(append(append([]*transcript.Message{}, c.Messages...), c.Unfired.Messages...), c.ToolResults...) {
		id, err := s.appendRow(ctx, c.SessionID, row)
		if err != nil {
			s.messages = beforeMessages
			s.positions = beforePositions
			s.nextID = beforeID
			return nil, err
		}
		result.MessageIDs = append(result.MessageIDs, id)
	}
	for _, output := range c.Outputs {
		result.Outputs = append(result.Outputs, &sessionstore.OutputCommit{Content: output.Content})
	}
	return result, nil
}

func (m *compactionMockLLM) Chat(
	_ context.Context,
	_ string,
	messages []llmwire.Message,
	_ []llmwire.ToolSchema,
	opts ...llmwire.ChatOption,
) (*llmwire.Response, error) {
	m.callCount++
	m.lastOptions = llmwire.ApplyChatOptions(opts)
	m.lastMessages = messages

	prompt := ""
	if len(messages) > 0 {
		prompt = messages[0].Content
	}

	m.prompts = append(m.prompts, prompt)

	if m.chat != nil {
		return m.chat(m.callCount, prompt)
	}

	if m.err != nil {
		return nil, m.err
	}
	return m.response, nil
}

func (m *compactionMockLLM) Model() string { return testMockModel }

func (m *compactionMockLLM) APIKey() string { return "" }

func (m *compactionMockLLM) Close() error { return nil }

func (m *compactionMockLLM) Provider() string { return testMockModel }

func (m *compactionMockLLM) ContextWindow() int { return m.contextWindow }

func (m *compactionMockLLM) SetReasoningLevel(string) {}

func (m *compactionMockLLM) SetImageAuthorizer(llm.ImageAuthorizer) {}

func (m *compactionMockLLM) GetReasoningLevel() string { return testReasoningLvl }

func (m *compactionMockLLM) SetSessionID(id string) {}

func (s *compactionRecordingStore) LoadActiveMessages(
	_ context.Context,
	sessionID int64,
) ([]*transcript.Message, error) {
	var active []*transcript.Message
	for _, message := range s.messages {
		if message.SessionID != sessionID || message.CompactedAt != nil {
			continue
		}

		stored := *message
		active = append(active, &stored)
	}

	slices.SortFunc(active, func(a, b *transcript.Message) int {
		return cmp.Compare(s.positions[a.ID], s.positions[b.ID])
	})

	return active, nil
}

func (c *countingTool) ID() string { return c.id }

func (c *countingTool) Description() string { return "counts" }

func (c *countingTool) Parameters() json.RawMessage { return json.RawMessage(`{}`) }

func (c *countingTool) ParallelSafe() bool { return false }

func (c *countingTool) Execute(context.Context, json.RawMessage) (*tool.Result, error) {
	c.runs.Add(1)

	return &tool.Result{Output: "ran"}, nil
}

func (m *mockSessionStore) Commit(_ context.Context, c sessionstore.Commit) (*sessionstore.CommitResult, error) {
	if c.Replace != nil {
		byID := map[int64]*transcript.Message{}
		for _, message := range m.messages {
			byID[message.ID] = message
		}
		m.messages = nil
		for _, entry := range c.Replace.Entries {
			if entry.ExistingID != 0 {
				m.messages = append(m.messages, byID[entry.ExistingID])
				continue
			}
			m.nextMsgID++
			row := *entry.Message
			row.ID = m.nextMsgID
			row.SessionID = c.SessionID
			m.messages = append(m.messages, &row)
		}
	}
	batch := append(append(append([]*transcript.Message{}, c.Messages...), c.Unfired.Messages...), c.ToolResults...)
	for range batch {
		m.insertCalls++
		if m.insertErr != nil && (m.insertFailAt == 0 || m.insertCalls == m.insertFailAt) {
			return nil, m.insertErr
		}
	}
	result := &sessionstore.CommitResult{}
	for _, message := range batch {
		m.nextMsgID++
		copyMessage := *message
		copyMessage.ID = m.nextMsgID
		copyMessage.SessionID = c.SessionID
		m.messages = append(m.messages, &copyMessage)
		result.MessageIDs = append(result.MessageIDs, copyMessage.ID)
	}
	for _, output := range append(c.Outputs, c.Unfired.Outputs...) {
		result.Outputs = append(result.Outputs, &sessionstore.OutputCommit{Content: output.Content})
	}
	for _, state := range []sessionstore.StatePatch{c.Unfired.State, c.State} {
		m.applyCandidate(state.Candidate, result.MessageIDs)

		if state.EmptyStopStreak != nil {
			m.completion.EmptyStopStreak = *state.EmptyStopStreak
		}
	}
	return result, nil
}

func (m *mockSessionStore) LoadActiveMessages(context.Context, int64) ([]*transcript.Message, error) {
	return m.messages, nil
}

func (*mockSessionStore) ListPending(context.Context, int64) ([]*sessionstore.InboxInput, error) {
	return nil, nil
}

func (*mockSessionStore) PendingActivation(context.Context, int64) (*sessionstore.ToolActivation, error) {
	return nil, sessionstore.ErrActivationNotFound
}

func (*mockSessionStore) HasBackgroundWakeSource(context.Context, int64) (bool, error) {
	return false, nil
}

func (m *mockSessionStore) LoadCompletionCheckState(
	context.Context,
	int64,
) (*sessionstore.CompletionCheckState, error) {
	return &m.completion, nil
}

func (*mockSessionStore) HasOutstandingResponseRecovery(context.Context, int64) (bool, error) {
	return false, nil
}

func (*mockSessionStore) GetChildSessionStats(context.Context, int64) (int, int, error) {
	return 0, 0, nil
}

func (*mockSessionStore) GetSessionTreeUsage(context.Context, int64) (int, int, float64, error) {
	return 0, 0, 0, nil
}

func (e noteEvents) Emit(n sessionevent.Notification) {
	if n.Type == sessionevent.NotifyMessage {
		*e.notes = append(*e.notes, n.Message)
	}
}

func (m *mockLLMRunOnce) Chat(
	context.Context,
	string,
	[]llmwire.Message,
	[]llmwire.ToolSchema,
	...llmwire.ChatOption,
) (*llmwire.Response, error) {
	m.called = true
	return normalizeScriptedResponse(m.response), nil
}

func (*mockLLMRunOnce) Model() string { return testMockModel }

func (*mockLLMRunOnce) APIKey() string { return "" }

func (*mockLLMRunOnce) Close() error { return nil }

func (*mockLLMRunOnce) Provider() string { return testMockModel }

func (*mockLLMRunOnce) ContextWindow() int { return 0 }

func (*mockLLMRunOnce) SetReasoningLevel(string) {}

func (*mockLLMRunOnce) GetReasoningLevel() string { return testReasoningLvl }

func (*mockLLMRunOnce) SetSessionID(string) {}

func (*mockLLMRunOnce) SetImageAuthorizer(llm.ImageAuthorizer) {}

func (m *loopScriptLLM) Chat(
	_ context.Context,
	_ string,
	messages []llmwire.Message,
	tools []llmwire.ToolSchema,
	_ ...llmwire.ChatOption,
) (*llmwire.Response, error) {
	m.calls++
	m.lastTools = tools
	m.lastMessages = messages
	if m.onCall != nil {
		r, err := m.onCall(m.calls, messages)
		return normalizeScriptedResponse(r), err
	}
	if m.err != nil {
		return nil, m.err
	}
	if m.calls > len(m.responses) {
		return nil, fmt.Errorf("unexpected model call %d", m.calls)
	}
	return normalizeScriptedResponse(m.responses[m.calls-1]), nil
}

func (*nilResultTool) ID() string { return "nil-result" }

func (*nilResultTool) Description() string { return "returns an invalid nil result" }

func (*nilResultTool) Parameters() json.RawMessage { return json.RawMessage(`{}`) }

func (*nilResultTool) ParallelSafe() bool { return false }

func (*nilResultTool) Execute(context.Context, json.RawMessage) (*tool.Result, error) {
	return nil, nil
}

func (s *stubTool) ParallelSafe() bool { return s.parallelSafe }

func (s *stubTool) ID() string { return s.id }

func (s *stubTool) Description() string { return "stub" }

func (s *stubTool) Parameters() json.RawMessage { return json.RawMessage(`{}`) }

func (s *stubTool) Execute(_ context.Context, _ json.RawMessage) (*tool.Result, error) {
	if s.err != nil {
		return nil, s.err
	}

	return &tool.Result{Output: s.result, IsError: s.resultIsError}, nil
}

func (c *trackedClient) Close() error { c.closed++; return nil }

func (c *trackedClient) SetSessionID(id string) { c.sessionID = id }

func (c *reasoningSwitchClient) Chat(
	ctx context.Context,
	system string,
	messages []llmwire.Message,
	tools []llmwire.ToolSchema,
	options ...llmwire.ChatOption,
) (*llmwire.Response, error) {
	c.requests = append(c.requests, messages)
	return c.mockLLMRunOnce.Chat(ctx, system, messages, tools, options...)
}

func (s *reasoningLoadStore) LoadActiveMessages(
	context.Context, int64,
) ([]*transcript.Message, error) {
	return s.messages, nil
}

func (m *statusStubStore) GetSessionTreeUsage(context.Context, int64) (int, int, float64, error) {
	return m.in, m.out, m.cost, nil
}

func (m *statusStubStore) GetChildSessionStats(context.Context, int64) (int, int, error) {
	return m.subagents, 0, nil
}

func (g *gateTool) ID() string { return g.id }

func (g *gateTool) Description() string { return "gate" }

func (g *gateTool) Parameters() json.RawMessage { return json.RawMessage(`{}`) }

func (g *gateTool) ParallelSafe() bool { return g.parallelSafe }

func (g *gateTool) Execute(_ context.Context, raw json.RawMessage) (*tool.Result, error) {
	var params struct {
		N string `json:"n"`
	}

	_ = json.Unmarshal(raw, &params)

	g.mu.Lock()

	g.entered = append(g.entered, params.N)
	err := g.errFor[params.N]
	release, gated := g.releases[len(g.entered)]
	g.mu.Unlock()

	if gated {
		<-release
	}

	if err != nil {
		if g.failTyped(params.N) {
			return &tool.Result{Output: err.Error(), IsError: true}, nil
		}

		return nil, err
	}

	return &tool.Result{Output: "ran:" + params.N}, nil
}

func newImagePlumbAgent(t *testing.T) (*Session, *imageStubTool) {
	t.Helper()

	_, store, sessionID := newAttachmentsStore(t)
	stub := &imageStubTool{id: "read", result: &tool.Result{
		Title:  "img.png",
		Output: "[img.png]\n<image>...</image>",
		Images: demoRefs,
	}}
	registry := tool.NewRegistry()
	registry.Register(stub)
	s := newTestAgent()
	s.id, s.rootID = sessionID, sessionID
	s.store, s.ms = store, newMessageStore(store, sessionID)
	s.registry = registry

	return s, stub
}

func newAttachmentsStore(t *testing.T) (*sql.DB, *sessionstore.Store, int64) {
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

// appendImageToolResult commits an assistant call plus its image-bearing tool
// result through the ordinary persistence path.
func appendImageToolResult(ctx context.Context, t *testing.T, ms *messageStore, cid string) {
	t.Helper()
	require.NoError(
		t,
		appendTestAssistant(
			ctx,
			ms,
			&llmwire.Response{Text: "step", ToolCalls: []llmwire.ToolCall{{ID: cid, Name: "read"}}},
		),
	)
	require.NoError(
		t,
		appendTestMessage(
			ctx,
			ms,
			&llmwire.Message{
				Role:       llmwire.RoleTool,
				Content:    "image loaded",
				ToolCallID: cid,
				ToolName:   "read",
				Images:     demoRefs,
			},
		),
	)
}

func assertBoundaryContinuationOrder(t *testing.T, messages []llmwire.Message, result string) {
	t.Helper()

	require.Len(t, messages, 5)
	assert.Equal(t, llmwire.RoleUser, messages[0].Role)
	assert.Equal(t, "original request", messages[0].Content)
	assert.Equal(t, llmwire.RoleAssistant, messages[1].Role)
	assert.Equal(t, llmwire.RoleTool, messages[2].Role)
	assert.Equal(t, "owned-call", messages[2].ToolCallID)
	assert.Equal(t, result, messages[2].Content)
	assert.Equal(t, llmwire.RoleUser, messages[3].Role)
	assert.Contains(t, messages[3].Content, "continue with the followup")
	assert.Equal(t, llmwire.RoleUser, messages[4].Role)
	assert.Equal(t, "active background producer snapshot", messages[4].Content)
}

// transcriptWithAbortedCall builds a pressure-crossing transcript whose oldest
// round is followed by an assistant row aborted mid-generation: a tool call
// with no id and nothing else on the row.
func transcriptWithAbortedCall(window int) []llmwire.Message {
	msgs := []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		compactionUserMessage("task"),
	}

	msgs = append(msgs, roundTokens("first", 20, 900)...)

	msgs = append(msgs, llmwire.Message{
		Role:      llmwire.RoleAssistant,
		ToolCalls: []llmwire.ToolCall{{ID: "", Name: "read"}},
	})

	for estimateTokens(msgs) < compactionCutoff(window)+10000 {
		msgs = append(msgs, roundTokens(fmt.Sprintf("big-%d", len(msgs)), 20, 900)...)
	}

	return msgs
}

// summarySizedForProjection returns summarizer text whose marked projection
// makes the post-commit estimate land on wantTailTokens summary tokens.
func summarySizedForProjection(t *testing.T, wantTailTokens int) string {
	t.Helper()

	for n := 4 * wantTailTokens; n > 0; n-- {
		if len(renderMarkedSummary(strings.Repeat("a", n), ""))/4 == wantTailTokens {
			return strings.Repeat("a", n)
		}
	}

	t.Fatal("no summary length lands on the target token count")

	return ""
}

func seedCompactableTranscript(ctx context.Context, t *testing.T, s *Session) {
	t.Helper()

	messages := []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		{Role: llmwire.RoleUser, Content: "task"},
	}
	for i := range 3 {
		messages = append(messages, roundTokens(fmt.Sprintf("c%d", i), 10, 10)...)
	}

	for i := range messages {
		message := messages[i]
		require.NoError(t, appendTestMessage(ctx, s.ms, &message))
	}
}

// findSummaryRow locates the committed marked summary row in a projection.
func findSummaryRow(t *testing.T, messages []llmwire.Message) llmwire.Message {
	t.Helper()

	for _, m := range messages {
		if isMarkedSummary(m.Content) {
			return m
		}
	}

	t.Fatal("marked summary not found")

	return llmwire.Message{}
}

// roundTokens builds an assistant-with-tool-call plus its result, sized in tokens.
func roundTokens(id string, callTokens, resultTokens int) []llmwire.Message {
	return []llmwire.Message{
		{
			Role:      llmwire.RoleAssistant,
			Content:   strings.Repeat("a", callTokens*4),
			ToolCalls: []llmwire.ToolCall{{ID: id, Name: "read"}},
		},
		{Role: llmwire.RoleTool, Content: strings.Repeat("t", resultTokens*4), ToolCallID: id, ToolName: "read"},
	}
}

func compactionUserMessage(content string) llmwire.Message {
	return llmwire.Message{Role: llmwire.RoleUser, Content: content}
}

func compactionAssistantCall(id, content string) llmwire.Message {
	return llmwire.Message{
		Role:      llmwire.RoleAssistant,
		Content:   content,
		ToolCalls: []llmwire.ToolCall{{ID: id, Name: "read"}},
	}
}

func compactionToolResult(id, content string) llmwire.Message {
	return llmwire.Message{Role: llmwire.RoleTool, Content: content, ToolCallID: id, ToolName: "read"}
}

func contextEventRunner(s *Session, notes *[]string) *runState {
	s.events = noteEvents{notes: notes}
	return &runState{}
}

// oversizedTranscript builds header + enough large rounds to cross the trigger.
func oversizedTranscript(window int) []llmwire.Message {
	messages := []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		compactionUserMessage("task"),
	}

	for estimateTokens(messages) < compactionCutoff(window)+10000 {
		messages = append(messages, roundTokens(fmt.Sprintf("big-%d", len(messages)), 20, 900)...)
	}

	return messages
}

func notesContain(notes []string, needle string) bool {
	return countNotes(notes, needle) > 0
}

func countNotes(notes []string, needle string) int {
	count := 0

	for _, n := range notes {
		if strings.Contains(n, needle) {
			count++
		}
	}

	return count
}

func (m *compactionProtocolModel) apply(command compactionCommand) {
	switch command {
	case cmdQueueCompact:
		m.queuedCompact = true
	case cmdStartExternalCall:
		if !m.externalPending && !m.workPending {
			m.externalPending = true
			m.freshContent = true
			m.rawRows++
		}
	case cmdDeliverExternalResult:
		if m.externalPending {
			m.externalPending = false
			m.freshContent = true
			m.rawRows++
			m.tailGroupRows = 2
		}
	case cmdEmitToolCall:
		if !m.externalPending && !m.workPending {
			m.workPending = true
			m.freshContent = true
			m.rawRows++
		}
	case cmdExecuteTools:
		if m.workPending {
			m.workPending = false
			m.freshContent = true
			m.rawRows++
			m.tailGroupRows = 2
		}
	case cmdSupersedeWithUserTurn:
		// A later user turn abandons a dangling ordinary call; an external call
		// keeps its producer and stays pending.
		m.workPending = false
		m.freshContent = true
		m.rawRows++
		m.tailGroupRows = 1
	case cmdRunLoopPoint:
		if !m.queuedCompact || m.externalPending || m.workPending {
			return
		}

		m.queuedCompact = false

		// A transcript holding only the previous compaction's own output has
		// nothing left to summarize, and the whole raw range is never the head:
		// at least one legal group stays verbatim.
		if m.freshContent && m.rawRows > m.tailGroupRows {
			m.compactionsHoped++
			m.freshContent = false
			m.rawRows = m.tailGroupRows
		}
	}
}

func runCompactionSequence(t *testing.T, sequence []compactionCommand) {
	t.Helper()

	llm := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: 200000,
	}
	s := newCompactionTestSvc(llm)
	s.stagedCalls = map[string]string{}
	setTestMessages(s, []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		compactionUserMessage("task"),
		compactionAssistantCall("seed", "work"),
		compactionToolResult("seed", "result"),
	})

	// The seeded round is one legal group: content a compaction could
	// summarize, but only once another row exists to keep verbatim.
	model := &compactionProtocolModel{
		freshContent:  true,
		rawRows:       2,
		tailGroupRows: 2,
	}
	next := 0

	var notes []string

	runner := contextEventRunner(s, &notes)

	for step, command := range sequence {
		next++

		before := llm.callCount
		pendingBefore := model.externalPending || model.workPending

		applyToSession(t, s, runner, command, next)
		model.apply(command)

		if llm.callCount > before {
			model.compactionsRun++

			assert.False(t, pendingBefore,
				"step %d (%v): compaction ran while a call was pending", step, command)
		}

		assert.Equal(t, model.externalPending, s.HasPendingExternalCall(),
			"step %d (%v): external-pending diverged from the model", step, command)
		assert.Equal(t, model.workPending, len(s.pendingInLoopCalls()) > 0,
			"step %d (%v): pending work diverged from the model", step, command)
	}

	assert.Equal(t, model.compactionsHoped, model.compactionsRun,
		"every compaction the model expects must have happened, and no others")
	s.ms.mu.Lock()
	queuedCompact := s.pendingCompaction
	s.ms.mu.Unlock()
	assert.Equal(t, model.queuedCompact, queuedCompact,
		"a queued /compact is neither dropped nor invented")
}

func applyToSession(t *testing.T, s *Session, runner *runState, command compactionCommand, seq int) {
	t.Helper()

	switch command {
	case cmdQueueCompact:
		s.RequestCompaction()
	case cmdStartExternalCall:
		if s.HasPendingExternalCall() || len(s.pendingInLoopCalls()) > 0 {
			return
		}

		id := fmt.Sprintf("ext-%d", seq)
		s.stagedCalls[id] = tool.IDTask
		appendMessages(t, s, llmwire.Message{
			Role:      llmwire.RoleAssistant,
			Content:   "spawning",
			ToolCalls: []llmwire.ToolCall{{ID: id, Name: tool.IDTask}},
		})
	case cmdDeliverExternalResult:
		for _, call := range s.PendingExternalCalls() {
			appendMessages(t, s, llmwire.Message{
				Role: llmwire.RoleTool, ToolCallID: call.ID, ToolName: call.Name, Content: "child done",
			})
		}
	case cmdEmitToolCall:
		if s.HasPendingExternalCall() || len(s.pendingInLoopCalls()) > 0 {
			return
		}

		appendMessages(t, s, compactionAssistantCall(fmt.Sprintf("work-%d", seq), "reading"))
	case cmdExecuteTools:
		for id := range unresolvedToolCalls(s.ms.getMessages()) {
			if s.stagedCalls[id] != "" {
				continue
			}

			appendMessages(t, s, llmwire.Message{
				Role: llmwire.RoleTool, ToolCallID: id, ToolName: "read", Content: "body",
			})
		}
	case cmdSupersedeWithUserTurn:
		appendMessages(t, s, compactionUserMessage(fmt.Sprintf("new instruction %d", seq)))
	case cmdRunLoopPoint:
		// Reaching the loop's single compaction point says nothing about safety —
		// deciding that is the production code's job, which is what this exercises.
		require.NoError(t, s.compactionStep(t.Context(), runner))
	}
}

func appendMessages(t *testing.T, s *Session, msgs ...llmwire.Message) {
	t.Helper()

	for _, message := range msgs {
		require.NoError(t, appendTestMessage(t.Context(), s.ms, &message))
	}
}

// compactionSequences enumerates every command sequence of exactly depth steps.
func compactionSequences(depth int) [][]compactionCommand {
	sequences := [][]compactionCommand{{}}

	for range depth {
		next := make([][]compactionCommand, 0, len(sequences)*len(compactionAlphabet))

		for _, prefix := range sequences {
			for _, command := range compactionAlphabet {
				extended := make([]compactionCommand, len(prefix), len(prefix)+1)
				copy(extended, prefix)
				next = append(next, append(extended, command))
			}
		}

		sequences = next
	}

	return sequences
}

func sequenceName(sequence []compactionCommand) string {
	var name strings.Builder

	for _, command := range sequence {
		name.WriteRune('A' + rune(command))
	}

	return name.String()
}

func mkStoredAssistant(callID string) llmwire.Message {
	return llmwire.Message{
		Role:      llmwire.RoleAssistant,
		ToolCalls: []llmwire.ToolCall{{ID: callID, Name: "subagent_event", Arguments: []byte(`{}`)}},
	}
}

func mkStoredResult(callID string) llmwire.Message {
	return llmwire.Message{
		Role: llmwire.RoleTool, ToolCallID: callID, ToolName: "subagent_event", Content: "child done",
	}
}

func countRowID(rowIDs []int64, id int64) int {
	count := 0

	for _, rowID := range rowIDs {
		if rowID == id {
			count++
		}
	}

	return count
}

func indexWithRowID(rowIDs []int64, id int64) int {
	for i, rowID := range rowIDs {
		if rowID == id {
			return i
		}
	}

	return -1
}

// pinFixture builds a transcript whose tail ends in the pending candidate and
// its host nudge: a header, a summarizable head, then two small rows. The head
// fits the request bound while the nudge alone clears the tail floor, so the
// unpinned split lands past the candidate and the pin is load-bearing.
func pinFixture(t *testing.T) ([]llmwire.Message, int) {
	t.Helper()

	messages := []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		{Role: llmwire.RoleUser, Content: "task"},
		{Role: llmwire.RoleAssistant, Content: strings.Repeat("h", 8_000)},
		{Role: llmwire.RoleAssistant, Content: "first answer"},
		{Role: llmwire.RoleUser, Content: "second look " + strings.Repeat("n", 4_000)},
	}

	pin := len(messages) - 2

	return messages, pin
}

func (s *compactionRecordingStore) appendRow(
	_ context.Context,
	sessionID int64,
	message *transcript.Message,
) (int64, error) {
	s.insertCalls++
	if s.insertErr != nil && (s.insertFailAt == 0 || s.insertCalls == s.insertFailAt) {
		return 0, s.insertErr
	}

	stored := *message
	stored.ID = s.nextID
	stored.SessionID = sessionID
	s.nextID++
	s.messages = append(s.messages, &stored)
	if s.positions == nil {
		s.positions = make(map[int64]int)
	}

	s.positions[stored.ID] = int(stored.ID)

	return stored.ID, nil
}

func (s *compactionRecordingStore) markRowsCompacted(_ context.Context, ids []int64) error {
	s.markCompacted++
	if s.markCompactedErr != nil {
		return s.markCompactedErr
	}

	now := time.Now()
	compacted := make(map[int64]bool, len(ids))
	for _, id := range ids {
		compacted[id] = true
	}

	for _, message := range s.messages {
		if compacted[message.ID] {
			message.CompactedAt = &now
		}
	}

	return nil
}

func (s *compactionRecordingStore) replaceRows(
	ctx context.Context,
	sessionID int64,
	compactedIDs []int64,
	entries []sessionstore.CompactionEntry,
) ([]int64, error) {
	if s.replaceErr != nil {
		return nil, s.replaceErr
	}

	if err := s.markRowsCompacted(ctx, compactedIDs); err != nil {
		return nil, err
	}

	ids := make([]int64, len(entries))
	for i, entry := range entries {
		id := entry.ExistingID
		if id == 0 {
			var err error

			id, err = s.appendRow(ctx, sessionID, entry.Message)
			if err != nil {
				return nil, err
			}
		}

		ids[i] = id
		s.positions[id] = i + 1
	}

	return ids, nil
}

func (s *Session) compactIfNeeded(ctx context.Context, window int) error {
	if !s.shouldCompact(window) {
		return nil
	}

	_, err := s.compact(ctx, nil)

	return err
}

func newCompactionTestSvc(mockLLM *compactionMockLLM) *Session {
	s := newTestAgent()
	s.llmClient = mockLLM
	return s
}

// findStoredSummary returns the persisted marked summary row.
func findStoredSummary(t *testing.T, store *compactionRecordingStore) *transcript.Message {
	t.Helper()

	for _, m := range store.messages {
		if isMarkedSummary(m.Content) {
			return m
		}
	}

	t.Fatal("summary row not found in store")

	return nil
}

func renderedSkills(messages []llmwire.Message) []llmwire.Message {
	var skills []llmwire.Message
	for _, message := range messages {
		for _, invocation := range loader.ExtractRenderedSkills(message.Content) {
			skillMessage := message
			skillMessage.Content = invocation.Envelope
			skills = append(skills, skillMessage)
		}
	}

	return skills
}

func skillMessage(t *testing.T, name, content string) llmwire.Message {
	t.Helper()

	return llmwire.Message{
		Role:    llmwire.RoleUser,
		Content: builtin.RenderSkill(&loader.Skill{Name: name, Content: content}, ""),
	}
}

func newTestAgent(tools ...tool.Tool) *Session {
	reg := tool.NewRegistry()
	for _, tt := range tools {
		reg.Register(tt)
	}
	store := &mockSessionStore{}
	prompt := sessionprompt.NewBuilder("test", "")
	prompt.Todos = todo.New()
	return &Session{
		id:           1,
		rootID:       1,
		llmClient:    &mockLLMRunOnce{response: textResponse("ok")},
		loader:       loader.New(),
		store:        store,
		ms:           newMessageStore(store, 1),
		loopDetector: newLoopDetector(),
		registry:     reg,
		prompt:       prompt,
	}
}

func setTestMessages(s *Session, messages []llmwire.Message) {
	store := s.ms.store
	switch m := store.(type) {
	case *mockSessionStore:
		m.messages = nil
		m.nextMsgID = 0
	case *compactionRecordingStore:
		m.messages = nil
		m.nextID = 1
		m.positions = nil
	}
	rows := make([]*transcript.Message, 0, len(messages))
	for i := range messages {
		row, err := storedMessage(&messages[i])
		if err != nil {
			panic(err)
		}
		rows = append(rows, row)
	}
	if _, err := store.Commit(context.Background(), sessionstore.Commit{SessionID: s.id, Messages: rows}); err != nil {
		panic(err)
	}
	s.store = store
	if err := s.ms.reloadMessages(context.Background()); err != nil {
		panic(err)
	}
}

func appendTestMessage(ctx context.Context, ms *messageStore, message *llmwire.Message) error {
	row, err := storedMessage(message)
	if err != nil {
		return err
	}
	if _, err := ms.store.Commit(
		ctx,
		sessionstore.Commit{SessionID: ms.sessID, Messages: []*transcript.Message{row}},
	); err != nil {
		return err
	}
	return ms.reloadMessages(ctx)
}

func appendTestUser(ctx context.Context, ms *messageStore, text string) error {
	return appendTestMessage(ctx, ms, &llmwire.Message{Role: llmwire.RoleUser, Content: text})
}

func appendTestAssistant(ctx context.Context, ms *messageStore, r *llmwire.Response) error {
	return appendTestMessage(
		ctx,
		ms,
		&llmwire.Message{
			Role:         llmwire.RoleAssistant,
			Content:      r.Text,
			ToolCalls:    r.ToolCalls,
			ReasoningRaw: r.ReasoningRaw,
		},
	)
}

func runTestLoop(t *testing.T, s *Session) (RunResult, error) {
	t.Helper()
	if len(s.ms.getMessages()) == 0 {
		setTestMessages(s, []llmwire.Message{usr("task")})
	}
	return s.Run(t.Context())
}

func textResponse(text string) *llmwire.Response {
	return &llmwire.Response{Text: text, FinishType: llmwire.FinishStop}
}

func toolCallResponse(id, name string) *llmwire.Response {
	return &llmwire.Response{
		ToolCalls:  []llmwire.ToolCall{{ID: id, Name: name, Arguments: json.RawMessage(`{}`)}},
		FinishType: llmwire.FinishToolCalls,
	}
}

func normalizeScriptedResponse(r *llmwire.Response) *llmwire.Response {
	if r != nil && r.FinishType == "" {
		copyResponse := *r
		if len(r.ToolCalls) > 0 {
			copyResponse.FinishType = llmwire.FinishToolCalls
		} else {
			copyResponse.FinishType = llmwire.FinishStop
		}
		return &copyResponse
	}
	return r
}

func usr(content string) llmwire.Message {
	return llmwire.Message{Role: llmwire.RoleUser, Content: content}
}

func asst(content string, calls ...llmwire.ToolCall) llmwire.Message {
	return llmwire.Message{Role: llmwire.RoleAssistant, Content: content, ToolCalls: calls}
}

func call(id, name string) llmwire.ToolCall {
	return llmwire.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(`{}`)}
}

func toolRes(id string) llmwire.Message {
	return llmwire.Message{Role: llmwire.RoleTool, ToolCallID: id, Content: "done"}
}

func transcriptText(messages []llmwire.Message) string {
	var out string
	for _, m := range messages {
		out += m.Content
	}
	return out
}

func hasSummaryRow(messages []llmwire.Message) bool {
	for _, m := range messages {
		if isMarkedSummary(m.Content) {
			return true
		}
	}
	return false
}

func (m *mockSessionStore) applyCandidate(change *sessionstore.CandidateChange, ids []int64) {
	if change == nil {
		return
	}
	next := change.Next
	if change.NextRef >= 0 {
		next = ids[change.NextRef]
	}
	m.completion.CandidateText = ""
	if next == 0 {
		m.completion.CandidateID = nil
		return
	}
	m.completion.CandidateID = &next
	for _, row := range m.messages {
		if row.ID == next {
			m.completion.CandidateText = row.Content
		}
	}
}

// pageScan builds one user message carrying one measured page-scan attachment
// (1615×2193 px, the incident's shape).
func pageScan(id string, rawBytes int64) llmwire.Message {
	return llmwire.Message{
		Role:    llmwire.RoleUser,
		Content: "page scan",
		Images: []llmwire.ImageRef{{
			Path: "/tmp/" + id + ".png", Mime: llmwire.MimeImagePng, Size: rawBytes,
			Width: 1615, Height: 2193,
		}},
	}
}

func buildMessagesWithTokens(tokens int) []llmwire.Message {
	totalChars := tokens * 4
	content := strings.Repeat("a", totalChars)
	return []llmwire.Message{{Role: llmwire.RoleUser, Content: content}}
}

func recordRounds(ld *loopDetector, rounds int, gen func(i int) toolRecord) {
	for i := range rounds {
		ld.record([]toolRecord{gen(i)})
	}
}

// readCalls builds n native read calls in one assistant turn.
func readCalls(n int) []llmwire.ToolCall {
	calls := make([]llmwire.ToolCall, 0, n)
	for i := range n {
		calls = append(calls, llmwire.ToolCall{
			ID:        fmt.Sprintf("read-%d", i),
			Name:      "read",
			Arguments: []byte(fmt.Sprintf(`{"path":"file-%d.go"}`, i)),
		})
	}

	return calls
}

func newGateTool(id string, parallelSafe bool) *gateTool {
	return &gateTool{
		id:           id,
		parallelSafe: parallelSafe,
		releases:     map[int]chan struct{}{},
		errFor:       map[string]error{},
		typedFor:     map[string]bool{},
	}
}

func (g *gateTool) failTyped(n string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.typedFor[n]
}

func (g *gateTool) gateAt(entryNumber int) chan struct{} {
	ch := make(chan struct{})
	g.mu.Lock()
	defer g.mu.Unlock()
	g.releases[entryNumber] = ch

	return ch
}

func (g *gateTool) fail(n string, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.errFor[n] = err
}

// failTyped makes the call return a typed tool.Result failure with the given
// payload instead of a Go error.
func (g *gateTool) setTypedFail(n string, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.errFor[n] = err
	g.typedFor[n] = true
}

func (g *gateTool) entryCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()

	return len(g.entered)
}

func gateCall(name, n string) llmwire.ToolCall {
	return llmwire.ToolCall{
		ID:        name + "-" + n,
		Name:      name,
		Arguments: json.RawMessage(`{"n":"` + n + `"}`),
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}

	t.Fatal("condition not met within deadline")
}

// Escaped names do not contain either complete boundary token.
func countUnescapedTokens(s, token string) int {
	return strings.Count(s, token)
}

// escapedTestPayload renders what the escaping contract must produce for the
// case's own fixture.
func escapedTestPayload(result *tool.Result) string {
	title := strings.ReplaceAll(result.Title, "UNTRUSTED_EXTERNAL_DATA", "UNTRUSTED_EXTERNAL_DATA_ESCAPED")
	output := strings.ReplaceAll(result.Output, "UNTRUSTED_EXTERNAL_DATA", "UNTRUSTED_EXTERNAL_DATA_ESCAPED")

	if title != "" {
		return "[" + title + "]\n" + output
	}

	return output
}

func requireUntrustedMarkerID(t *testing.T, content string) string {
	t.Helper()
	marker := regexp.MustCompile(`<<<BEGIN_UNTRUSTED_EXTERNAL_DATA id="([0-9a-f]{16})">>>`)
	matches := marker.FindAllStringSubmatch(content, -1)
	require.Len(t, matches, 1)

	return matches[0][1]
}
