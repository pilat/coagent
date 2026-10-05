package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/configapply"
	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/mcpstore"
	"github.com/pilat/coagent/internal/progressruntime"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/sessionbuild"
	"github.com/pilat/coagent/internal/sessionbus"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/tool/builtin"
)

// Record the registry schemas actually delivered to the session's model.
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

// Responses inspect the real system prompt and transcript to preserve scenario ordering.
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
	// Cancellation must preempt a blocked responder; responder panics return to the loop's recovery boundary.
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

		// A nil scripted response must behave like a real provider error.
		if o.resp == nil {
			return nil, errors.New("scripted provider failure")
		}

		// Unparsed scripted responses need the completion outcome normally supplied by provider parsing.
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

// Recorded Chat contexts expose any child lifetime deadline incorrectly added by the runner.
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

// fakeMCPServer is one on-disk stdio server plus the log that records its spawns.
type fakeMCPServer struct {
	path    string
	log     string
	pong    string
	release string
}

// A held server delays every tool call until release, exposing cancellation and restart ordering.
func newFakeMCPServer(t *testing.T, pong string, held bool) *fakeMCPServer {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake MCP server is a POSIX shell script")
	}
	dir := t.TempDir()
	f := &fakeMCPServer{path: filepath.Join(dir, "fakemcp.sh"), log: filepath.Join(dir, "events.log"), pong: pong}
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

// Goroutine observers cannot use testing.T; a log read failure must remain observable as -1.
func (f *fakeMCPServer) countNoFail(event string) int {
	data, err := os.ReadFile(f.log)
	if err != nil {
		return -1
	}
	return strings.Count(string(data), event+"\n")
}

type providerRequest struct {
	Model     string            `json:"model"`
	SessionID string            `json:"session_id"`
	Messages  []providerMessage `json:"messages"`
	Reasoning struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
	Tools []struct {
		Function llmwire.ToolSchema `json:"function"`
	} `json:"tools"`
}

type scriptedClientKey struct {
	sessionID, model string
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
	observers ...func(providerRequest),
) sessionbuild.BuildInput {
	t.Helper()
	if cfg.WorkDir == "" {
		cfg.WorkDir = t.TempDir()
	}
	var mu sync.Mutex
	clients := make(map[scriptedClientKey]llm.Client)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request providerRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		for _, observe := range observers {
			if observe != nil {
				observe(request)
			}
		}
		mu.Lock()
		clientKey := scriptedClientKey{request.SessionID, request.Model}
		client := clients[clientKey]
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
			clients[clientKey] = client
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
					message.ToolCalls, llmwire.ToolCall{
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
		if request.Reasoning.Effort != "" {
			client.SetReasoningLevel(request.Reasoning.Effort)
		}
		response, err := client.Chat(r.Context(), system, messages, schemas)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var calls []map[string]any
		for index, call := range response.ToolCalls {
			calls = append(
				calls, map[string]any{
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
		ctx, in, store, links, budgets, backgroundprocess.NewStore(db, store), progressruntime.New(store, bus), bus,
		schedules, in.Config, mcp, applier,
	).(*svc)
	return service, service.processes
}

func lifecycleInput(ctx context.Context, t *testing.T, s *svc, id int64, command string) *sessionstore.InboxInput {
	t.Helper()
	input, err := s.enqueueUserSessionInput(ctx, id, command)
	require.NoError(t, err)
	return input
}

func enqueueCallResult(ctx context.Context, store Store, id int64, callID, name, content string) (bool, error) {
	key := "result:" + callID
	if name == "config_edit" {
		key = "config_apply:" + callID
	}
	result, err := store.Enqueue(
		ctx, sessionstore.Input{
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

// Record reasoning effort per model to expose each session's actual request.
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

// Production claims are acknowledged in assertion and recording modes, preserving ack-triggered readiness events.
func drainScenarioClaims(t *testing.T, name string, controller controllerapi.OutputQueueController) {
	t.Helper()
	path := harnessTracePath(name)
	file := harnessTraceFile{SourceTest: t.Name()}
	if *updateHarnessTraces {
		if data, err := os.ReadFile(path); err == nil {
			require.NoError(t, json.Unmarshal(data, &file))
			file.SourceTest = t.Name()
		}

		// One drain corresponds to one scenario run: recorded claims are
		// replaced, never accumulated across recording passes.
		file.Claims = nil
	}
	receipts := map[string]string{}
	placeholderSeq := 0
	for {
		claim, err := controller.ClaimOutput(t.Context())
		if errors.Is(err, controllerapi.ErrNoOutput) {
			break
		}
		require.NoError(t, err, "production claim must succeed while recording")
		if *updateHarnessTraces {
			recorded := harnessTraceClaim{
				Type:                         claim.Type,
				Content:                      normalizeClaimContent(claim.Content),
				Attributes:                   sanitizeClaimAttributes(t, claim.Attributes),
				SourceKey:                    claim.SourceKey,
				ModelInputGeneration:         claim.ModelInputGeneration,
				PreviousMessageType:          claim.PreviousMessageType,
				PreviousModelInputGeneration: claim.PreviousModelInputGeneration,
				ReleasesInput:                claim.ReleasesInput,
			}
			for _, raw := range previousMessageIDs(claim) {
				placeholder, ok := receipts[raw]
				if !ok {
					placeholderSeq++
					placeholder = fmt.Sprintf("%s%d>", messagePlaceholderPrefix, placeholderSeq)
					receipts[raw] = placeholder
				}
				recorded.PreviousMessageIDs = append(recorded.PreviousMessageIDs, placeholder)
			}
			file.Claims = append(file.Claims, recorded)
		}
		if claim.Type == controllerapi.OutputMessageReplaceable ||
			claim.Type == controllerapi.OutputMessagePersistent {
			require.NoError(t, controller.AckOutput(t.Context(), controllerapi.OutputAckData{
				ID: claim.ID, AttemptID: claim.AttemptID,
				MessageIDs: []string{fmt.Sprintf("recorded-%d", claim.ID)},
			}), "production ack must succeed while recording")
		} else {
			require.NoError(t, controller.AckOutput(t.Context(), controllerapi.OutputAckData{
				ID: claim.ID, AttemptID: claim.AttemptID,
			}))
		}
	}
	if *updateHarnessTraces {
		writeHarnessTrace(t, path, file)
	}
}

// Wall time varies between runs; other card content must remain exact in golden claims.
func normalizeClaimContent(content string) string {
	return elapsedPattern.ReplaceAllString(content, "⌚ <elapsed>")
}

// Temporary paths and progress hashes carry no conversation meaning and must not destabilize recorded claims.
func sanitizeClaimAttributes(t *testing.T, attributes map[string]any) map[string]any {
	t.Helper()
	sanitized := make(map[string]any, len(attributes))
	for key, value := range attributes {
		if key == "progress_revision" {
			continue
		}
		if key == "work_dir" {
			sanitized[key] = workDirPlaceholder
			continue
		}
		sanitized[key] = value
	}
	if len(sanitized) == 0 {
		return nil
	}
	return sanitized
}

func previousMessageIDs(claim *controllerapi.OutputClaimData) []string {
	values, ok := claim.PreviousMessageAttributes["message_ids"].([]any)
	if !ok {
		return nil
	}
	ids := make([]string, 0, len(values))
	for _, value := range values {
		id, ok := value.(string)
		if !ok {
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

// Keep wake-time formatting exact so production layout changes break the golden.
const (
	waitTimeLayout     = "15:04 02 Jan"
	wakePlaceholder    = "<wake>"
	workDirPlaceholder = "<workdir>"
	// Normalize the temporary project path; it carries no scenario meaning.
	namePlaceholder = "<name>"
)

// The -update-traces flag records new golden traces instead of asserting existing ones.
var updateHarnessTraces = flag.Bool(
	"update-traces", false, "rewrite the recorded controller traces under internal/testdata",
)

var childRefPattern = regexp.MustCompile(`#(\d+)`)

// Elapsed card time depends on scheduling and carries no conversation meaning.
var elapsedPattern = regexp.MustCompile(`⌚ [0-9.]+[a-z0-9.]+`)

// Shared traces preserve ordered notifications and claims; receipt IDs normalize, generations stay exact.
type harnessTraceFile struct {
	SourceTest string              `json:"source_test"`
	Trace      []harnessTraceEvent `json:"trace"`
	Claims     []harnessTraceClaim `json:"claims,omitempty"`
}

// Telegram replay maps normalized receipts to real targets so edits and chunk bookkeeping use valid message IDs.
const messagePlaceholderPrefix = "<msg-"

// Claim generations stay exact; random attempt IDs and raw receipts do not survive normalization.
type harnessTraceClaim struct {
	Type                         string         `json:"type"`
	Content                      string         `json:"content"`
	Attributes                   map[string]any `json:"attributes,omitempty"`
	SourceKey                    string         `json:"source_key,omitempty"`
	ModelInputGeneration         *int64         `json:"model_input_generation,omitempty"`
	PreviousMessageType          string         `json:"previous_message_type,omitempty"`
	PreviousModelInputGeneration *int64         `json:"previous_model_input_generation,omitempty"`
	PreviousMessageIDs           []string       `json:"previous_message_ids,omitempty"`
	ReleasesInput                bool           `json:"releases_input"`
}

type harnessTraceEvent struct {
	Type       string             `json:"type"`
	Message    string             `json:"message,omitempty"`
	Status     string             `json:"status,omitempty"`
	Reason     string             `json:"reason,omitempty"`
	Source     string             `json:"source,omitempty"`
	Name       string             `json:"name,omitempty"`
	WorkDir    string             `json:"work_dir,omitempty"`
	Attributes map[string]any     `json:"attributes,omitempty"`
	Waiting    []harnessTraceWait `json:"waiting,omitempty"`
}

type harnessTraceWait struct {
	Kind  string `json:"kind"`
	Child string `json:"child,omitempty"`
	Wake  string `json:"wake,omitempty"`
}

func assertHarnessTrace(
	t *testing.T,
	name string,
	events []controllerapi.SessionNotification,
	sessionID int64,
) {
	t.Helper()
	assertHarnessTraceForScenario(t, t.Name(), name, events, sessionID)
}

func assertHarnessTraceForScenario(
	t *testing.T,
	sourceTest, name string,
	events []controllerapi.SessionNotification,
	sessionID int64,
) {
	t.Helper()
	got := harnessTraceFile{SourceTest: sourceTest, Trace: normalizeHarnessTrace(t, events, sessionID)}
	path := harnessTracePath(name)
	if *updateHarnessTraces {
		stored := readHarnessTraceFileForUpdate(t, path)
		stored.SourceTest = sourceTest
		stored.Trace = got.Trace
		writeHarnessTrace(t, path, stored)
		return
	}
	want := readHarnessTraceFile(t, path)
	assert.Equal(t, want.SourceTest, got.SourceTest, "golden %s belongs to another scenario", name)
	assert.Equal(t, want.Trace, got.Trace, "controller-visible notification trace")
}

// Daemon and manager tests must consume the same trace artifacts to prevent contract drift.
func harnessTracePath(name string) string {
	return filepath.Join("..", "testdata", "harness_scenarios", name)
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

// Recording may read an artifact containing only the notification pass.
func readHarnessTraceFileForUpdate(t *testing.T, path string) harnessTraceFile {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err, "run the notification trace recording before claim recording")
	var file harnessTraceFile
	require.NoError(t, json.Unmarshal(data, &file))
	return file
}

func writeHarnessTrace(t *testing.T, path string, file harnessTraceFile) {
	t.Helper()
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	require.NoError(t, encoder.Encode(file))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o600))
	t.Logf("recorded controller trace %s", path)
}

func normalizeHarnessTrace(
	t *testing.T,
	events []controllerapi.SessionNotification,
	sessionID int64,
) []harnessTraceEvent {
	t.Helper()
	children := map[int64]string{}
	wakes := []string{}
	out := make([]harnessTraceEvent, 0, len(events))
	for _, event := range events {
		if event.SessionID != sessionID {
			continue
		}
		n := event.Notification
		if n.Type == sessionevent.NotifyHeartbeat {
			// Periodic heartbeat counts vary with wall time and only drive typing, so deterministic goldens exclude them.
			continue
		}
		requireRecordableNotification(t, n)
		recorded := harnessTraceEvent{
			Type:   string(n.Type),
			Status: string(n.Status),
			Reason: n.Reason,
			Source: n.Source,
		}
		if len(n.Attributes) > 0 {
			recorded.Attributes = n.Attributes
		}
		if n.Name != "" {
			recorded.Name = namePlaceholder
		}
		if n.WorkDir != "" {
			recorded.WorkDir = workDirPlaceholder
		}
		for _, item := range n.Waiting {
			wait := harnessTraceWait{Kind: string(item.Kind)}
			switch item.Kind {
			case sessionevent.WaitSubagent:
				wait.Child = childRef(children, item.ChildID)
			case sessionevent.WaitSleep:
				wait.Wake = wakePlaceholder
				wakes = append(wakes, item.WakeAt.Local().Format(waitTimeLayout))
			}
			recorded.Waiting = append(recorded.Waiting, wait)
		}
		recorded.Message = normalizeHarnessMessage(n.Message, children, wakes)
		out = append(out, recorded)
	}
	return out
}

// Unsupported notification payloads must fail recording rather than silently disappear.
func requireRecordableNotification(t *testing.T, n sessionevent.Notification) {
	t.Helper()
	require.Zero(t, n.OldSessionID, "extend the trace schema before recording session clears")
	require.Zero(t, n.NewSessionID, "extend the trace schema before recording session clears")
}

func childRef(children map[int64]string, id int64) string {
	if ref, ok := children[id]; ok {
		return ref
	}
	ref := fmt.Sprintf("#%d", len(children)+1)
	children[id] = ref
	return ref
}

func normalizeHarnessMessage(message string, children map[int64]string, wakes []string) string {
	if message == "" {
		return ""
	}
	message = elapsedPattern.ReplaceAllString(message, "⌚ <elapsed>")
	for _, wake := range wakes {
		message = strings.ReplaceAll(message, wake, wakePlaceholder)
	}
	return childRefPattern.ReplaceAllStringFunc(message, func(match string) string {
		for id, ref := range children {
			if match == fmt.Sprintf("#%d", id) {
				return ref
			}
		}
		return match
	})
}
