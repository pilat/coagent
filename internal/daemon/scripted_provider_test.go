package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/configapply"
	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/mcpstore"
	"github.com/pilat/coagent/internal/progressruntime"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/sessionbuild"
	"github.com/pilat/coagent/internal/sessionbus"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/tool/builtin"
)

// recordingLLM is the scripted client that reports what the session's registry
// produced — the boundary a user's model actually sees.
type recordingLLM struct {
	scriptedLLM

	rec *schemaRecorder

	mu        sync.Mutex
	sessionID int64
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

type registryPromptLLM struct {
	scriptedLLM
	recorder *activationSchemas
	prompts  *promptRecorder

	mu        sync.Mutex
	sessionID int64
}

func (c *registryPromptLLM) SetSessionID(id string) {
	if index := strings.LastIndex(id, ":"); index >= 0 {
		id = id[index+1:]
	}

	parsed, _ := strconv.ParseInt(id, 10, 64)
	c.mu.Lock()
	c.sessionID = parsed
	c.mu.Unlock()
}

func (c *registryPromptLLM) Chat(
	ctx context.Context,
	system string,
	messages []llmwire.Message,
	tools []llmwire.ToolSchema,
	opts ...llmwire.ChatOption,
) (*llmwire.Response, error) {
	c.mu.Lock()
	sessionID := c.sessionID
	c.mu.Unlock()

	c.prompts.record(strconv.FormatInt(sessionID, 10), system)
	c.recorder.record(sessionID, tools)

	return c.scriptedLLM.Chat(ctx, system, messages, tools, opts...)
}

func scheduleRestartResponder(release <-chan struct{}) func(string, []llmwire.Message) *llmwire.Response {
	return func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasToolResultFor(messages, tool.IDSchedule) {
			<-release

			return &llmwire.Response{Text: "scheduled work completed"}
		}

		return &llmwire.Response{Text: "ready for schedule"}
	}
}

// scriptedLLM is a fake llm.Client whose responses are produced by a
// test-supplied function inspecting the system prompt and messages.
type scriptedLLM struct {
	respond func(system string, msgs []llmwire.Message) *llmwire.Response

	mu          sync.Mutex
	sessionID   string
	chatContext *contextInfo // the ctx of the most recent in-flight Chat
	cancelSeen  bool         // a Chat returned because its ctx was cancelled
}

func (c *scriptedLLM) Chat(
	ctx context.Context,
	system string,
	msgs []llmwire.Message,
	_ []llmwire.ToolSchema,
	_ ...llmwire.ChatOption,
) (*llmwire.Response, error) {
	// Run respond off the loop goroutine so a ctx deadline/cancel (a blocking
	// child's timeout, or a kill) preempts a respond that blocks — mirroring a real
	// client honoring ctx. A panic in respond is re-raised on the caller (the
	// session loop goroutine) so its panic-recovery can mark the child errored.
	type outcome struct {
		resp  *llmwire.Response
		panic any
	}

	_, hasDeadline := ctx.Deadline()
	c.mu.Lock()
	c.chatContext = &contextInfo{hasDeadline: hasDeadline}
	c.mu.Unlock()

	ch := make(chan outcome, 1)

	go func() {
		defer func() {
			if p := recover(); p != nil {
				ch <- outcome{panic: p}
			}
		}()

		ch <- outcome{resp: c.respond(system, msgs)}
	}()

	select {
	case <-ctx.Done():
		c.mu.Lock()
		c.cancelSeen = true
		c.mu.Unlock()

		return nil, ctx.Err()
	case o := <-ch:
		if o.panic != nil {
			panic(o.panic)
		}

		// A nil response is the scripted way to say "provider failure": return
		// it as an error, exactly as a real failing client would.
		if o.resp == nil {
			return nil, errors.New("scripted provider failure")
		}

		// The scripted harness bypasses provider parsing, so give an unparsed
		// response the normal completion outcome a real client would report.
		if o.resp.FinishType == "" {
			if len(o.resp.ToolCalls) > 0 {
				o.resp.FinishType = llmwire.FinishToolCalls
			} else {
				o.resp.FinishType = llmwire.FinishStop
			}
		}

		return o.resp, nil
	}
}

func (c *scriptedLLM) Model() string { return "fake-model" }

func (c *scriptedLLM) APIKey() string { return "" }

func (c *scriptedLLM) Close() error { return nil }

func (c *scriptedLLM) Provider() string { return "fake" }

func (c *scriptedLLM) ContextWindow() int { return 200000 }

func (c *scriptedLLM) SetReasoningLevel(_ string) {}

func (c *scriptedLLM) SetImageAuthorizer(llm.ImageAuthorizer) {}

func (c *scriptedLLM) GetReasoningLevel() string { return "medium" }

func (c *scriptedLLM) SetSessionID(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.sessionID = id
}

// chatRanWithDeadline reports whether any Chat of this client ran under a ctx
// that carried a deadline — the child-lifetime deadline the runner must not add.
func (c *scriptedLLM) chatRanWithDeadline() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.chatContext != nil && c.chatContext.hasDeadline
}

// hasChatContext reports whether the client has observed at least one Chat.
func (c *scriptedLLM) hasChatContext() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.chatContext != nil
}

func (c *scriptedLLM) sawCancellation() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.cancelSeen
}

func newShiftingMCPServer(t *testing.T, pong string) *fakeMCPServer {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("the fake MCP server is a POSIX shell script")
	}

	dir := t.TempDir()
	f := &fakeMCPServer{
		path: filepath.Join(dir, "shiftingmcp.sh"),
		log:  filepath.Join(dir, "events.log"),
		pong: pong,
	}

	require.NoError(t, os.WriteFile(f.path, []byte(shiftingMCPScript), 0o700))
	require.NoError(t, os.WriteFile(f.log, nil, 0o600))

	return f
}

const exitTrackingMCPScript = `#!/bin/sh
LOG="$1"
PONG="$2"
(
  parent=$$
  while kill -0 "$parent" 2>/dev/null; do sleep 0.01; done
  echo exit >> "$LOG"
) &
echo spawn >> "$LOG"
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  [ -n "$id" ] || continue
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2024-11-05","capabilities":{"tools":{}},"serverInfo":{"name":"exit-mcp","version":"0.0.1"}}}\n' "$id"
      ;;
    *'"method":"tools/list"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"ping","description":"Answers pong.","inputSchema":{"type":"object","properties":{}}}]}}\n' "$id"
      ;;
    *'"method":"tools/call"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"%s"}]}}\n' "$id" "$PONG"
      ;;
    *)
      printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"Method not found"}}\n' "$id"
      ;;
  esac
done
`

type exitTrackingMCPServer struct {
	path string
	log  string
	pong string
}

func newExitTrackingMCPServer(t *testing.T, pong string) *exitTrackingMCPServer {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake MCP server is a POSIX shell script")
	}

	dir := t.TempDir()
	fake := &exitTrackingMCPServer{
		path: filepath.Join(dir, "exit-mcp.sh"),
		log:  filepath.Join(dir, "events.log"),
		pong: pong,
	}
	require.NoError(t, os.WriteFile(fake.path, []byte(exitTrackingMCPScript), 0o700))
	require.NoError(t, os.WriteFile(fake.log, nil, 0o600))

	return fake
}

func (f *exitTrackingMCPServer) args() []string {
	return []string{f.log, f.pong}
}

func (f *exitTrackingMCPServer) count(t *testing.T, event string) int {
	t.Helper()
	data, err := os.ReadFile(f.log)
	require.NoError(t, err)

	return strings.Count(string(data), event+"\n")
}

func (f *exitTrackingMCPServer) waitForExit(t *testing.T) {
	f.waitForExitCount(t, 1)
}

func (f *exitTrackingMCPServer) waitForExitCount(t *testing.T, want int) {
	t.Helper()
	require.Eventually(t, func() bool {
		return f.count(t, "exit") >= want
	}, 5*time.Second, 10*time.Millisecond, "MCP subprocesses did not exit")
}

func exitTrackingParams(t *testing.T, fake *exitTrackingMCPServer) string {
	t.Helper()
	data, err := json.Marshal(fake.args())
	require.NoError(t, err)

	return `{"name":"fake","scope":"project","command":"` + fake.path + `","args":` + string(data) + `}`
}

// fakeMCPServer is one on-disk stdio server plus the log that records its spawns.
type fakeMCPServer struct {
	path    string
	log     string
	pong    string
	release string
}

// newFakeMCPServer writes a server that answers with pong. A held server blocks
// every tools/call until release() is called.
func newFakeMCPServer(t *testing.T, pong string, held bool) *fakeMCPServer {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("the fake MCP server is a POSIX shell script")
	}

	dir := t.TempDir()
	f := &fakeMCPServer{
		path: filepath.Join(dir, "fakemcp.sh"),
		log:  filepath.Join(dir, "events.log"),
		pong: pong,
	}

	if held {
		f.release = filepath.Join(dir, "release")
	}

	require.NoError(t, os.WriteFile(f.path, []byte(fakeMCPScript), 0o700))
	require.NoError(t, os.WriteFile(f.log, nil, 0o600))

	return f
}

func (f *fakeMCPServer) addParams(name, scope string) string {
	args, err := json.Marshal([]string{f.log, f.pong, f.release})
	if err != nil {
		panic(err)
	}

	return `{"name":"` + name + `","scope":"` + scope + `","command":"` + f.path + `","args":` + string(args) + `}`
}

func (f *fakeMCPServer) unblock(t *testing.T) {
	t.Helper()
	require.NoError(t, os.WriteFile(f.release, nil, 0o600))
}

func (f *fakeMCPServer) count(t *testing.T, event string) int {
	t.Helper()

	data, err := os.ReadFile(f.log)
	require.NoError(t, err)

	return strings.Count(string(data), event+"\n")
}

// countNoFail samples the log without a testing.T, for observers that run on
// session goroutines (scripted LLM responders). A read error returns -1.
func (f *fakeMCPServer) countNoFail(event string) int {
	data, err := os.ReadFile(f.log)
	if err != nil {
		return -1
	}

	return strings.Count(string(data), event+"\n")
}

func disableScenarioResponder(fake *fakeMCPServer) func(string, []llmwire.Message) *llmwire.Response {
	return func(_ string, messages []llmwire.Message) *llmwire.Response {
		last := lastUserText(messages)
		switch {
		case strings.Contains(last, "USE_AFTER_RESTART"):
			if hasToolResultForCallID(messages, "ping-after-restart") {
				return &llmwire.Response{Text: "used after restart"}
			}
			return mcpPingCall("ping-after-restart")
		case strings.Contains(last, "USE_IT"):
			if hasToolResultForCallID(messages, "ping-before-restart") {
				return &llmwire.Response{Text: "used before restart"}
			}
			return mcpPingCall("ping-before-restart")
		case strings.Contains(last, "DISABLE_IT"):
			if hasToolResultFor(messages, tool.IDMCPDisable) {
				return &llmwire.Response{Text: "disabled"}
			}
			return mcpToolCall("disable-1", tool.IDMCPDisable, `{"name":"fake","scope":"project"}`)
		default:
			if hasToolResultFor(messages, tool.IDMCPAdd) {
				return &llmwire.Response{Text: "registered"}
			}
			return mcpToolCall("add-1", tool.IDMCPAdd, fake.addParams("fake", "project"))
		}
	}
}

func removeScenarioResponder(
	t *testing.T,
	fake *exitTrackingMCPServer,
) func(string, []llmwire.Message) *llmwire.Response {
	t.Helper()

	return func(_ string, messages []llmwire.Message) *llmwire.Response {
		last := lastUserText(messages)
		switch {
		case strings.Contains(last, "USE_AFTER_REMOVE"):
			if hasToolResultForCallID(messages, "ping-after-remove") {
				return &llmwire.Response{Text: "used after remove"}
			}

			return mcpPingCall("ping-after-remove")
		case strings.Contains(last, "USE_IT"):
			if hasToolResultForCallID(messages, "ping-before-remove") {
				return &llmwire.Response{Text: "used before remove"}
			}

			return mcpPingCall("ping-before-remove")
		case strings.Contains(last, "REMOVE_IT"):
			if hasToolResultFor(messages, tool.IDMCPRemove) {
				return &llmwire.Response{Text: "removed"}
			}

			return mcpToolCall("remove-1", tool.IDMCPRemove, `{"name":"fake","scope":"project"}`)
		default:
			if hasToolResultFor(messages, tool.IDMCPAdd) {
				return &llmwire.Response{Text: "registered"}
			}

			return mcpToolCall("add-1", tool.IDMCPAdd, exitTrackingParams(t, fake))
		}
	}
}

func lengthRecoveryResponder(t *testing.T) func(string, []llmwire.Message) *llmwire.Response {
	t.Helper()
	var calls int

	return func(_ string, messages []llmwire.Message) *llmwire.Response {
		calls++
		if calls > 1 {
			visible := scenarioTranscriptText(messages)
			require.NotContains(t, visible, "rejected private fragment")
			require.Contains(t, visible, sessionstore.OutputLengthRecoveryPrompt)

			return &llmwire.Response{Text: "recovered complete answer", FinishType: llmwire.FinishStop}
		}

		return &llmwire.Response{
			Text: "rejected private fragment", FinishType: llmwire.FinishLength,
			ProviderFinishReason: "length", CostUSD: 0.5,
			Usage: &llmwire.MessageUsage{PromptTokens: 100, CompletionTokens: 200},
			ToolCalls: []llmwire.ToolCall{
				{
					ID: "side-effect-probe", Name: "todowrite",
					Arguments: []byte(
						`{"items":[{"id":"must-not-run","content":"must not run","status":"in_progress","priority":"high"}]}`,
					),
				},
				{ID: "truncated-call", Name: "bash", Arguments: []byte(
					`{"command":"` + strings.Repeat("x", 128*1024),
				)},
			},
		}
	}
}

func stoppedRootScheduleResponder(
	tc stoppedRootScheduleCase,
	started chan<- struct{},
	release <-chan struct{},
) func(string, []llmwire.Message) *llmwire.Response {
	// The completion check calls the model twice for one scheduled turn; only
	// the first call arms the signal.
	var once sync.Once

	return func(_ string, messages []llmwire.Message) *llmwire.Response {
		if scheduledTurnRequested(tc, messages) {
			once.Do(func() { close(started) })
			<-release

			return &llmwire.Response{Text: tc.answer}
		}

		return &llmwire.Response{Text: "ready"}
	}
}

type providerRequest struct {
	Model     string            `json:"model"`
	SessionID string            `json:"session_id"`
	Messages  []providerMessage `json:"messages"`
	Tools     []struct {
		Function llmwire.ToolSchema `json:"function"`
	} `json:"tools"`
}

type providerMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"tool_call_id"`
	Name       string          `json:"name"`
	ToolCalls  []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

func scriptedBuildInput(
	t *testing.T,
	cfg *config.Config,
	store *sessionstore.Store,
	mcp mcpstore.Store,
	clientFor func(*config.Config) (llm.Client, error),
) sessionbuild.BuildInput {
	t.Helper()
	if cfg.WorkDir == "" {
		cfg.WorkDir = t.TempDir()
	}
	var mu sync.Mutex
	clients := make(map[string]llm.Client)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request providerRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		client := clients[request.SessionID]
		if client == nil {
			view := *cfg
			view.Model = request.Model
			var err error
			client, err = clientFor(&view)
			if err != nil {
				mu.Unlock()
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			client.SetSessionID(request.SessionID)
			clients[request.SessionID] = client
		}
		mu.Unlock()
		var system string
		var messages []llmwire.Message
		for _, row := range request.Messages {
			var content string
			if err := json.Unmarshal(row.Content, &content); err != nil {
				var parts []struct {
					Text string `json:"text"`
				}
				_ = json.Unmarshal(row.Content, &parts)
				for _, part := range parts {
					content += part.Text
				}
			}
			if row.Role == "system" {
				system = content
				continue
			}
			message := llmwire.Message{Role: row.Role, Content: content, ToolCallID: row.ToolCallID, ToolName: row.Name}
			for _, call := range row.ToolCalls {
				message.ToolCalls = append(
					message.ToolCalls,
					llmwire.ToolCall{
						ID:        call.ID,
						Name:      call.Function.Name,
						Arguments: json.RawMessage(call.Function.Arguments),
					},
				)
			}
			messages = append(messages, message)
		}
		var schemas []llmwire.ToolSchema
		for _, entry := range request.Tools {
			schemas = append(schemas, entry.Function)
		}
		response, err := client.Chat(r.Context(), system, messages, schemas)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var calls []map[string]any
		for index, call := range response.ToolCalls {
			calls = append(
				calls,
				map[string]any{
					"index":    index,
					"id":       call.ID,
					"type":     "function",
					"function": map[string]any{"name": call.Name, "arguments": string(call.Arguments)},
				},
			)
		}
		finish := "stop"
		if response.FinishType == llmwire.FinishToolCalls || len(calls) > 0 {
			finish = "tool_calls"
		}
		if response.FinishType == llmwire.FinishLength {
			finish = "length"
		}
		if response.FinishType == llmwire.FinishUnknown {
			finish = response.ProviderFinishReason
			if finish == "" {
				finish = "unknown"
			}
		}
		usage := map[string]any{"cost": response.CostUSD}
		if response.Usage != nil {
			usage["prompt_tokens"] = response.Usage.PromptTokens
			usage["completion_tokens"] = response.Usage.CompletionTokens
		}
		body, marshalErr := json.Marshal(
			map[string]any{
				"choices": []map[string]any{
					{
						"index":         0,
						"delta":         map[string]any{"content": response.Text, "tool_calls": calls},
						"finish_reason": finish,
					},
				},
				"usage": usage,
			},
		)
		if marshalErr != nil {
			http.Error(w, marshalErr.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", body)
	}))
	t.Cleanup(server.Close)
	if cfg.UnifiedConfig == nil {
		cfg.UnifiedConfig = &config.UnifiedConfig{}
	}
	if cfg.UnifiedConfig.Providers == nil {
		cfg.UnifiedConfig.Providers = make(map[string]config.ProviderEntry)
	}
	cfg.UnifiedConfig.Providers["scripted"] = config.ProviderEntry{
		Driver:  "openrouter",
		APIKey:  "fixture",
		BaseURL: server.URL,
	}
	if len(cfg.UnifiedConfig.Models) == 0 {
		cfg.UnifiedConfig.Models = []config.ModelEntry{{ID: cfg.Model, ContextWindow: 200000}}
	}
	for index := range cfg.UnifiedConfig.Models {
		cfg.UnifiedConfig.Models[index].Provider = "scripted"
	}
	return sessionbuild.BuildInput{Config: cfg, Store: store, MCPStore: mcp, Resources: builtin.NewResources()}
}

func newScenarioDaemon(
	ctx context.Context,
	in sessionbuild.BuildInput,
	store *sessionstore.Store,
	links subagent.Store,
	budgets budget.Service,
	schedules schedule.Service,
	model func() string,
	db *sql.DB,
) (*svc, backgroundprocess.Service) {
	bus := sessionbus.New()
	defaultModel := "fake-model"
	if model != nil {
		defaultModel = model()
	}
	if budgets == nil {
		budgets = budget.New(store)
	}
	if schedules == nil {
		schedules = schedule.NewService(schedule.NewStore(db, store), store)
	}
	in.Config.Model = defaultModel
	mcp := in.MCPStore
	if mcp == nil {
		mcp = mcpstore.NewStore(db)
	}
	in.MCPStore = mcp
	applier := configapply.New(
		configops.New(filepath.Join(in.Config.WorkDir, "config.yaml"), filepath.Join(in.Config.WorkDir, "secrets")),
		store,
	)
	service := New(
		ctx,
		in,
		store,
		links,
		budgets,
		backgroundprocess.NewStore(db, store),
		progressruntime.New(store, bus),
		bus,
		schedules,
		in.Config,
		mcp,
		applier,
	).(*svc)
	return service, service.processes
}

func lifecycleInput(ctx context.Context, t *testing.T, s *svc, id int64, command string) *sessionstore.InboxInput {
	t.Helper()
	input, err := s.enqueueUserSessionInput(ctx, id, command)
	require.NoError(t, err)
	return input
}

func enqueueScheduledInput(ctx context.Context, store Store, id int64, key, content string, fresh bool) (bool, error) {
	result, err := store.Enqueue(
		ctx,
		sessionstore.Input{
			SessionID:   id,
			Source:      sessionstore.InputSourceSchedule,
			Content:     content,
			DeliveryKey: key,
			Attributes:  map[string]any{"fresh": fresh},
		},
	)
	if err != nil {
		return false, err
	}
	return result.Applied, nil
}

func enqueueCallResult(ctx context.Context, store Store, id int64, callID, name, content string) (bool, error) {
	key := "result:" + callID
	if name == "config_edit" {
		key = "config_apply:" + callID
	}
	result, err := store.Enqueue(
		ctx,
		sessionstore.Input{
			SessionID:   id,
			Source:      sessionstore.InputSourceCallResult,
			Content:     content,
			Attributes:  map[string]any{"call_id": callID, "tool_id": name},
			DeliveryKey: key,
		},
	)
	if err != nil {
		return false, err
	}
	return result.Applied, nil
}

// effortProvider is an OpenRouter-shaped endpoint recording the reasoning effort
// of every call, so what a session actually asks for is observable.
type effortProvider struct {
	url string

	mu       sync.Mutex
	requests []effortRequest
}

func newEffortProvider(t *testing.T) *effortProvider {
	t.Helper()

	p := &effortProvider{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model     string `json:"model"`
			Reasoning struct {
				Effort string `json:"effort"`
			} `json:"reasoning"`
		}

		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))

		p.mu.Lock()
		p.requests = append(p.requests, effortRequest{model: body.Model, effort: body.Reasoning.Effort})
		turn := len(p.requests)
		p.mu.Unlock()

		_, _ = fmt.Fprintf(w,
			`{"choices":[{"message":{"role":"assistant","content":"answer %d"},"finish_reason":"stop"}],"usage":{}}`,
			turn,
		)
	}))
	t.Cleanup(srv.Close)

	p.url = srv.URL

	return p
}

func (p *effortProvider) snapshot() []effortRequest {
	p.mu.Lock()
	defer p.mu.Unlock()

	return append([]effortRequest(nil), p.requests...)
}

// spawnEffortProvider is an OpenRouter-shaped endpoint recording the reasoning
// effort of every call per model, so what each session asks for is observable.
type spawnEffortProvider struct {
	url string

	mu      sync.Mutex
	efforts map[string]string
}

func newSpawnEffortProvider(t *testing.T) *spawnEffortProvider {
	t.Helper()

	p := &spawnEffortProvider{efforts: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model     string `json:"model"`
			Reasoning struct {
				Effort string `json:"effort"`
			} `json:"reasoning"`
		}

		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)

			return
		}

		p.mu.Lock()
		p.efforts[body.Model] = body.Reasoning.Effort
		p.mu.Unlock()

		_, _ = fmt.Fprint(w,
			`{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{}}`,
		)
	}))
	t.Cleanup(srv.Close)

	p.url = srv.URL

	return p
}

func (p *spawnEffortProvider) effortFor(model string) string {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.efforts[model]
}
