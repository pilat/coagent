package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/mcp"
	"github.com/pilat/coagent/internal/mcpstore"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/transcript"
)

// scriptedLLM is a fake llm.Client whose responses are produced by a
// test-supplied function inspecting the system prompt and messages.
type scriptedLLM struct {
	respond func(system string, msgs []llmwire.Message) *llmwire.Response

	mu          sync.Mutex
	sessionID   string
	chatContext *contextInfo // the ctx of the most recent in-flight Chat
	cancelSeen  bool         // a Chat returned because its ctx was cancelled
}

// contextInfo snapshots the seam observation the raw client can make: what the
// runner actually handed the session loop.
type contextInfo struct {
	hasDeadline bool
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
		if o.resp.FinishType == "" && len(o.resp.ToolCalls) == 0 {
			o.resp.FinishType = llmwire.FinishStop
		}

		return o.resp, nil
	}
}

func (c *scriptedLLM) Model() string              { return "fake-model" }
func (c *scriptedLLM) APIKey() string             { return "" }
func (c *scriptedLLM) Close() error               { return nil }
func (c *scriptedLLM) Provider() string           { return "fake" }
func (c *scriptedLLM) ContextWindow() int         { return 200000 }
func (c *scriptedLLM) SetReasoningLevel(_ string) {}
func (c *scriptedLLM) GetReasoningLevel() string  { return "medium" }
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

// subagentHarness wires a real session.Factory (fake LLM) + daemon svc over a
// temp SQLite DB.
type subagentHarness struct {
	t          *testing.T
	db         *sql.DB
	mgr        *svc
	sessStore  sessionstore.Store
	links      subagent.Store
	schedStore schedule.Store
	projectID  int64
	ctx        context.Context

	llmMu   sync.Mutex
	llmRefs []*scriptedLLM // every client the session factory created
}

// sessionClient returns the scripted client bound to the session whose
// SetSessionID matched id — the raw seam between runner and session loop.
func (h *subagentHarness) sessionClient(sessionID string) *scriptedLLM {
	h.llmMu.Lock()
	defer h.llmMu.Unlock()

	for _, c := range h.llmRefs {
		c.mu.Lock()
		id := c.sessionID
		c.mu.Unlock()

		if id == sessionID {
			return c
		}
	}

	return nil
}

const taskCallID = "task-call-1"

// respond drives both parent and child sessions. A user message containing
// "CHILD_TASK" marks the child conversation; the child does one tool round
// before finishing, so its transcript holds two raw groups and a /compact on
// it has something to summarize besides the never-empty tail.
func subagentRespond(_ string, msgs []llmwire.Message) *llmwire.Response {
	if isCompactionPrompt(msgs) {
		return &llmwire.Response{Text: "child checkpoint: ran ls, finished 42"}
	}

	if hasUserContaining(msgs, "CHILD_TASK") {
		if hasToolResultFor(msgs, "ls") {
			return &llmwire.Response{Text: "child finished: 42"}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID:        "child-ls-1",
			Name:      "ls",
			Arguments: []byte(`{"path":"."}`),
		}}}
	}

	if hasToolResultFor(msgs, "task") || hasToolResultFor(msgs, "subagent_event") {
		return &llmwire.Response{Text: "all set, child launched"}
	}

	return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
		ID:   taskCallID,
		Name: "task",
		Arguments: []byte(
			`{"prompt":"CHILD_TASK do the thing","description":"child work","subagent_type":"general","background":true}`,
		),
	}}}
}

func newSubagentHarness(t *testing.T) *subagentHarness {
	return newSubagentHarnessWith(t, subagentRespond)
}

func newSubagentHarnessWith(
	t *testing.T,
	respond func(system string, msgs []llmwire.Message) *llmwire.Response,
) *subagentHarness {
	t.Helper()

	return newSubagentHarnessDecorated(t, respond, nil)
}

// newSubagentHarnessDecorated is newSubagentHarnessWith plus an injection point
// for the link store, so ledger-failure paths can be exercised on a live daemon.
func newSubagentHarnessDecorated(
	t *testing.T,
	respond func(system string, msgs []llmwire.Message) *llmwire.Response,
	decorate func(subagent.Store) subagent.Store,
) *subagentHarness {
	t.Helper()

	return newSubagentHarnessOnDB(t, filepath.Join(t.TempDir(), "test.db"), respond, decorate)
}

// newSubagentHarnessOnDB builds a harness over an explicit database file, so a
// test can shut one daemon down and bring another up on the same durable state.
func newSubagentHarnessOnDB(
	t *testing.T,
	dbPath string,
	respond func(system string, msgs []llmwire.Message) *llmwire.Response,
	decorate func(subagent.Store) subagent.Store,
) *subagentHarness {
	return newSubagentHarnessOnDBWithProject(t, dbPath, respond, decorate, false)
}

func newSubagentHarnessOnSystemProjectDB(
	t *testing.T,
	dbPath string,
	respond func(system string, msgs []llmwire.Message) *llmwire.Response,
) *subagentHarness {
	return newSubagentHarnessOnDBWithProject(t, dbPath, respond, nil, true)
}

func newSubagentHarnessOnDBWithProject(
	t *testing.T,
	dbPath string,
	respond func(system string, msgs []llmwire.Message) *llmwire.Response,
	decorate func(subagent.Store) subagent.Store,
	systemProject bool,
) *subagentHarness {
	t.Helper()

	db, err := migrate.OpenDB(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(context.Background(), db, dbPath))

	store := NewStore(db)
	sessStore := sessionstore.NewStore(db)
	links := subagent.NewStore(db)
	schedStore := schedule.NewStore(db)

	if decorate != nil {
		links = decorate(links)
	}

	workDir := t.TempDir()
	if systemProject {
		workDir = filepath.Join(workDir, controllerapi.CoagentSystemProjectDir)
		require.NoError(t, os.MkdirAll(workDir, 0o755))
	}
	cfg := &config.Config{WorkDir: workDir, Model: "fake-model"}

	var (
		pid int64
		h   = &subagentHarness{
			t: t, db: db, sessStore: sessStore, links: links, schedStore: schedStore,
			ctx: context.Background(),
		}
	)
	if systemProject {
		pid, err = store.GetOrCreateSystemProject(
			context.Background(), workDir, controllerapi.CoagentSystemProjectName,
		)
	} else {
		pid, err = store.GetOrCreateProject(context.Background(), workDir)
	}
	require.NoError(t, err)

	factory := session.NewFactoryWithOptions(
		cfg, nil, nil, sessStore, sessStore, nil, nil, nil, nil, nil,
		session.WithLLMClientFactory(func(_ *config.Config) (llm.Client, error) {
			client := &scriptedLLM{respond: respond}

			h.llmMu.Lock()
			h.llmRefs = append(h.llmRefs, client)
			h.llmMu.Unlock()

			return client, nil
		}),
	)

	mgr := newSvc(
		factory,
		store,
		sessStore,
		sessStore,
		sessStore,
		sessStore,
		sessStore,
		sessStore,
		sessStore,
		links,
		subagent.NewTransactions(db),
		budget.New(sessStore),
		sessStore,
		schedule.NewService(schedStore),
		func() string { return "fake-model" },
	)
	if systemProject {
		mgr.systemProject = workDir
	}

	h.mgr = mgr
	h.projectID = pid

	return h
}

func hasAssistantToolCall(msgs []llmwire.Message, toolName string) bool {
	for _, m := range msgs {
		if m.Role != llmwire.RoleAssistant {
			continue
		}

		for _, tc := range m.ToolCalls {
			if tc.Name == toolName {
				return true
			}
		}
	}

	return false
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

func (h *subagentHarness) shutdown() { h.mgr.Shutdown(5 * time.Second) }

func (h *subagentHarness) waitForChildLink(parentID int64) subagent.Link {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		link, err := h.links.GetLinkByTaskCallID(h.ctx, parentID, taskCallID)
		require.NoError(h.t, err)

		if link != nil {
			return *link
		}

		time.Sleep(10 * time.Millisecond)
	}

	h.t.Fatalf("timed out waiting for child link of parent %d", parentID)

	return subagent.Link{}
}

func (h *subagentHarness) waitForDelivery(childID int64) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		link, err := h.links.GetLink(h.ctx, childID)
		require.NoError(h.t, err)

		if link != nil && link.DeliveredAt != 0 {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	h.t.Fatalf("timed out waiting for completion delivery of child %d", childID)
}

func (h *subagentHarness) parentMessages(parentID int64) []llmwire.Message {
	h.t.Helper()
	stored, err := h.sessStore.LoadActiveMessages(h.ctx, parentID)
	require.NoError(h.t, err)

	return toDTO(stored)
}

func toDTO(stored []*transcript.Message) []llmwire.Message {
	msgs := make([]llmwire.Message, 0, len(stored))

	for _, m := range stored {
		msg := llmwire.Message{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID, ToolName: m.ToolName}
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

func lastToolResultContent(msgs []llmwire.Message, toolName string) string {
	for _, v := range slices.Backward(msgs) {
		if v.Role == llmwire.RoleTool && v.ToolName == toolName {
			return v.Content
		}
	}

	return ""
}

func countSubagentEvents(msgs []llmwire.Message, childID int64) int {
	needle := "\"child_id\":" + strconv.FormatInt(childID, 10)
	count := 0

	for _, m := range msgs {
		if m.Role != llmwire.RoleAssistant {
			continue
		}

		for _, tc := range m.ToolCalls {
			if tc.Name == "subagent_event" && strings.Contains(string(tc.Arguments), needle) {
				count++
			}
		}
	}

	return count
}

func TestIntegration_BackgroundSubagentCompletes(t *testing.T) {
	h := newSubagentHarness(t)
	defer h.shutdown()

	parentID, err := h.mgr.Send(h.ctx, h.projectID, "SPAWN_CHILD please", "fake-model", nil)
	require.NoError(t, err)

	link := h.waitForChildLink(parentID)
	assert.Equal(t, parentID, link.ParentID)
	assert.Equal(t, taskCallID, link.TaskCallID)
	assert.False(t, link.Blocking)

	// Regression guard for the cascade-kill change (#12): an *idle* (not killed)
	// parent must still survive its background child and be revived by the child's
	// completion — only a deliberately killed tree drops background descendants.
	h.waitForDelivery(link.ChildID)

	// Parent transcript is a valid tool_use/tool_result pairing.
	msgs := h.parentMessages(parentID)
	require.NoError(t, llm.ValidateToolPairing(msgs), "parent transcript must be transcript-valid")

	// Exactly one completion record for the child.
	assert.Equal(t, 1, countSubagentEvents(msgs, link.ChildID), "exactly one completion record")

	// get_subagent_result returns completed + output, and the auto-delivered
	// completion shows the SAME formatted string as get_subagent_result.
	res, err := h.mgr.Result(h.ctx, link.ChildID)
	require.NoError(t, err)
	assert.True(t, res.Terminal)
	assert.Equal(t, subagent.StateCompleted, res.State)
	assert.Equal(t, subagent.OutcomeCompleted, res.Outcome)
	assert.Contains(t, res.Output, "child finished")
	assert.Equal(t, formatChildResult(res), lastToolResultContent(msgs, "subagent_event"),
		"auto-delivered completion and get_subagent_result format identically")
}

// A model-authored task+sleep batch is not a valid join: both tools execute
// concurrently, so the sleep cannot order the child and creates a competing wake
// protocol. The harness launches the background child but rejects sleep before
// it stages a timer; child completion remains the sole wake source.
func TestIntegration_BackgroundTaskRejectsCompetingSleepProtocol(t *testing.T) {
	childRelease := make(chan struct{})
	const sleepCallID = "sleep-call-125"

	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "CHILD_TASK") {
			<-childRelease
			return &llmwire.Response{Text: "child finished while parent slept"}
		}

		if hasToolResultFor(msgs, "subagent_event") {
			return &llmwire.Response{Text: "child completion handled"}
		}

		if hasToolResultFor(msgs, tool.IDSleep) {
			return &llmwire.Response{Text: "background launched; yielding without sleep"}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{
			{
				ID:   taskCallID,
				Name: tool.IDTask,
				Arguments: []byte(
					`{"prompt":"CHILD_TASK wait for release","description":"child work","subagent_type":"general","background":true}`,
				),
			},
			{
				ID:        sleepCallID,
				Name:      tool.IDSleep,
				Arguments: []byte(`{"duration":"1h","reason":"wait for child"}`),
			},
		}}
	}

	h := newSubagentHarnessWith(t, respond)
	defer h.shutdown()

	parentID, err := h.mgr.Send(h.ctx, h.projectID, "spawn and wait", "fake-model", nil)
	require.NoError(t, err)
	link := h.waitForChildLink(parentID)

	h.waitUntil("parent yields without sleeping", func() bool { return !h.mgr.HasActiveLoop(parentID) })

	schedules, err := h.schedStore.ListSchedules(h.ctx, parentID)
	require.NoError(t, err)
	require.Empty(t, schedules, "rejected sleep must stage no timer")

	close(childRelease)
	h.waitForDelivery(link.ChildID)
	h.mgr.waitIdle(parentID)

	msgs := h.parentMessages(parentID)
	require.NoError(t, llm.ValidateToolPairing(msgs), "the whole transcript must remain provider-valid")
	assert.Equal(t, 1, countAssistantToolCallsFor(msgs, tool.IDSleep))
	assert.Equal(t, 1, countToolResultsFor(msgs, tool.IDSleep), "the rejected call gets one error result")
	assert.Equal(t, 1, countSubagentEvents(msgs, link.ChildID), "the child completion is delivered exactly once")

	var sleepResult *llmwire.Message
	for i := range msgs {
		if msgs[i].Role == llmwire.RoleTool && msgs[i].ToolName == tool.IDSleep {
			sleepResult = &msgs[i]
			break
		}
	}
	require.NotNil(t, sleepResult)
	assert.Equal(t, sleepCallID, sleepResult.ToolCallID, "the result must target the model's original call id")
	assert.Contains(t, sleepResult.Content, "sleep cannot be combined with task")

	schedules, err = h.schedStore.ListSchedules(h.ctx, parentID)
	require.NoError(t, err)
	assert.Empty(t, schedules)
}

// This is the composition-boundary contract, not another executor unit test:
// real schedule.Executor calls the daemon's producer-owned SessionSender,
// daemon queues an exact result, and session attaches it to the original call.
func TestIntegration_SchedulerWakesExactSleepThroughDaemonQueue(t *testing.T) {
	const sleepCallID = "sleep-call-scheduler-boundary"

	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasToolResultFor(msgs, tool.IDSleep) {
			return &llmwire.Response{Text: "timer handled"}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID:        sleepCallID,
			Name:      tool.IDSleep,
			Arguments: []byte(`{"duration":"1ms","reason":"boundary test"}`),
		}}}
	}

	h := newSubagentHarnessWith(t, respond)
	defer h.shutdown()

	parentID, err := h.mgr.Send(h.ctx, h.projectID, "sleep briefly", "fake-model", nil)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		schedules, listErr := h.schedStore.ListSchedules(h.ctx, parentID)

		return listErr == nil && len(schedules) == 1 && !h.mgr.HasActiveLoop(parentID)
	}, 5*time.Second, 10*time.Millisecond, "sleep must be durable before the scheduler fires")

	executor := schedule.NewExecutor(h.schedStore, h.mgr)
	executor.Start(h.ctx)
	defer executor.Stop()

	require.Eventually(t, func() bool {
		schedules, listErr := h.schedStore.ListSchedules(h.ctx, parentID)
		if listErr != nil || len(schedules) != 0 {
			return false
		}

		for _, msg := range h.parentMessages(parentID) {
			if msg.Role == llmwire.RoleTool && msg.ToolCallID == sleepCallID {
				return !h.mgr.HasActiveLoop(parentID)
			}
		}

		return false
	}, 5*time.Second, 10*time.Millisecond, "scheduler must commit the exact result before removing its ledger")

	msgs := h.parentMessages(parentID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Equal(t, 1, countAssistantToolCallsFor(msgs, tool.IDSleep))
	assert.Equal(t, 1, countToolResultsFor(msgs, tool.IDSleep))

	var result *llmwire.Message
	for i := range msgs {
		if msgs[i].Role == llmwire.RoleTool && msgs[i].ToolName == tool.IDSleep {
			result = &msgs[i]
			break
		}
	}

	require.NotNil(t, result)
	assert.Equal(t, sleepCallID, result.ToolCallID)
	assert.Contains(t, result.Content, "Sleep completed")

	schedules, err := h.schedStore.ListSchedules(h.ctx, parentID)
	require.NoError(t, err)
	assert.Empty(t, schedules, "accepted one-shot must be removed only after transcript delivery")
}

func TestIntegration_UserInterruptCancelsSleepWithoutDeletingStandaloneOneShot(t *testing.T) {
	const sleepCallID = "sleep-call-user-interrupt"

	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasToolResultFor(msgs, tool.IDSleep) {
			return &llmwire.Response{Text: "interrupt handled"}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID:        sleepCallID,
			Name:      tool.IDSleep,
			Arguments: []byte(`{"duration":"1h","reason":"boundary test"}`),
		}}}
	}

	h := newSubagentHarnessWith(t, respond)
	defer h.shutdown()

	parentID, err := h.mgr.Send(h.ctx, h.projectID, "sleep until interrupted", "fake-model", nil)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		schedules, listErr := h.schedStore.ListSchedules(h.ctx, parentID)

		return listErr == nil && len(schedules) == 1 && !h.mgr.HasActiveLoop(parentID)
	}, 5*time.Second, 10*time.Millisecond, "sleep must suspend with one durable timer")

	standaloneAt := time.Now().Add(2 * time.Hour).UTC()
	_, err = h.schedStore.AddSchedule(
		h.ctx, parentID, "", &standaloneAt, "standalone future input", false,
	)
	require.NoError(t, err)

	require.NoError(t, h.mgr.SendToSession(h.ctx, parentID, "interrupt now"))
	h.mgr.waitIdle(parentID)

	messages := h.parentMessages(parentID)
	require.NoError(t, llm.ValidateToolPairing(messages))
	assert.Equal(t, 1, countToolResultsFor(messages, tool.IDSleep))
	assert.Contains(t, lastToolResultContent(messages, tool.IDSleep), "Sleep interrupted")
	assert.Equal(t, "interrupt handled", lastAssistantTextDTO(messages))

	schedules, err := h.schedStore.ListSchedules(h.ctx, parentID)
	require.NoError(t, err)
	require.Len(t, schedules, 1, "interrupt must remove only the pending sleep timer")
	assert.Equal(t, "standalone future input", schedules[0].InputMessage())
}

func TestIntegration_StandaloneOneShotFlowsThroughExecutorAndDaemonQueue(t *testing.T) {
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasToolResultFor(msgs, tool.IDSchedule) {
			return &llmwire.Response{Text: "one-shot handled"}
		}

		return &llmwire.Response{Text: "ready"}
	}

	h := newSubagentHarnessWith(t, respond)
	defer h.shutdown()

	parentID, err := h.mgr.Send(h.ctx, h.projectID, "initialize", "fake-model", nil)
	require.NoError(t, err)
	h.mgr.waitIdle(parentID)

	due := time.Now().Add(-time.Minute).UTC()
	_, err = h.schedStore.AddSchedule(
		h.ctx, parentID, "", &due, "one-time scheduled work", false,
	)
	require.NoError(t, err)

	executor := schedule.NewExecutor(h.schedStore, h.mgr)
	executor.Start(h.ctx)
	defer executor.Stop()

	require.Eventually(t, func() bool {
		schedules, listErr := h.schedStore.ListSchedules(h.ctx, parentID)
		if listErr != nil || len(schedules) != 0 || h.mgr.HasActiveLoop(parentID) {
			return false
		}

		return lastAssistantTextDTO(h.parentMessages(parentID)) == "one-shot handled"
	}, 5*time.Second, 10*time.Millisecond)

	messages := h.parentMessages(parentID)
	require.NoError(t, llm.ValidateToolPairing(messages))
	assert.Equal(t, 1, countToolResultsFor(messages, tool.IDSchedule))
	assert.Contains(t, lastToolResultContent(messages, tool.IDSchedule), "one-time scheduled work")
}

type failFirstRemoveScheduleStore struct {
	schedule.Store
	once      sync.Once
	attempted chan struct{}
}

func (s *failFirstRemoveScheduleStore) RemoveSchedule(ctx context.Context, id int64) error {
	failed := false
	s.once.Do(func() {
		failed = true
		close(s.attempted)
	})
	if failed {
		return assert.AnError
	}

	return s.Store.RemoveSchedule(ctx, id)
}

func TestIntegration_OneShotAckFailureRedeliversWithoutDuplicateTranscriptOrPublication(t *testing.T) {
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasToolResultFor(msgs, tool.IDSchedule) {
			return &llmwire.Response{Text: "one-shot handled once"}
		}

		return &llmwire.Response{Text: "ready"}
	}

	h := newSubagentHarnessWith(t, respond)
	defer h.shutdown()

	parentID, err := h.mgr.Send(h.ctx, h.projectID, "initialize", "fake-model", nil)
	require.NoError(t, err)
	h.mgr.waitIdle(parentID)

	due := time.Now().Add(-time.Minute).UTC()
	_, err = h.schedStore.AddSchedule(
		h.ctx, parentID, "", &due, "one-time retry-safe work", false,
	)
	require.NoError(t, err)

	sub := h.mgr.PubSub().Subscribe(parentID)
	defer h.mgr.PubSub().Unsubscribe(parentID, sub)

	flaky := &failFirstRemoveScheduleStore{
		Store: h.schedStore, attempted: make(chan struct{}),
	}
	first := schedule.NewExecutor(flaky, h.mgr)
	first.Start(h.ctx)
	select {
	case <-flaky.attempted:
	case <-time.After(5 * time.Second):
		t.Fatal("first executor did not reach the failing acknowledgement")
	}
	first.Stop()
	h.mgr.waitIdle(parentID)

	remaining, err := h.schedStore.ListSchedules(h.ctx, parentID)
	require.NoError(t, err)
	require.Len(t, remaining, 1, "failed producer ack must leave the one-shot retryable")

	second := schedule.NewExecutor(h.schedStore, h.mgr)
	second.Start(h.ctx)
	require.Eventually(t, func() bool {
		schedules, listErr := h.schedStore.ListSchedules(h.ctx, parentID)
		return listErr == nil && len(schedules) == 0 && !h.mgr.HasActiveLoop(parentID)
	}, 5*time.Second, 10*time.Millisecond)
	second.Stop()

	messages := h.parentMessages(parentID)
	require.NoError(t, llm.ValidateToolPairing(messages))
	assert.Equal(t, 1, countToolResultsFor(messages, tool.IDSchedule))
	assert.Equal(t, "one-shot handled once", lastAssistantTextDTO(messages))

	scheduledPublications := 0
	for {
		select {
		case notification := <-sub:
			if notification.Type == sessionevent.NotifyInputReceived && notification.Source == "scheduler" {
				scheduledPublications++
			}
		default:
			assert.Equal(t, 1, scheduledPublications, "ack retry must not republish accepted input")
			return
		}
	}
}

func TestIntegration_FreshScheduleDuplicateDoesNotResetOrRunTwice(t *testing.T) {
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "fresh scheduled work") {
			return &llmwire.Response{Text: "fresh handled once"}
		}

		return &llmwire.Response{Text: "ready"}
	}

	h := newSubagentHarnessWith(t, respond)
	defer h.shutdown()

	parentID, err := h.mgr.Send(h.ctx, h.projectID, "initialize", "fake-model", nil)
	require.NoError(t, err)
	h.mgr.waitIdle(parentID)

	const deliveryID = "schedule:cron:23:20260814T1200Z"
	applied, err := h.mgr.DeliverFreshSchedule(
		h.ctx, parentID, deliveryID, "fresh scheduled work",
	)
	require.NoError(t, err)
	assert.True(t, applied)
	h.mgr.waitIdle(parentID)

	applied, err = h.mgr.DeliverFreshSchedule(
		h.ctx, parentID, deliveryID, "fresh scheduled work",
	)
	require.NoError(t, err)
	assert.False(t, applied)
	h.mgr.waitIdle(parentID)

	messages := h.parentMessages(parentID)
	require.NoError(t, llm.ValidateToolPairing(messages))
	assert.Equal(t, 1, countMessageContentContaining(messages, "fresh scheduled work"))
	assert.Equal(t, 1, countMessageContentContaining(messages, "fresh handled once"))
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

func TestIntegration_SendToSubagentReNotifies(t *testing.T) {
	h := newSubagentHarness(t)
	defer h.shutdown()

	parentID, err := h.mgr.Send(h.ctx, h.projectID, "SPAWN_CHILD please", "fake-model", nil)
	require.NoError(t, err)

	link := h.waitForChildLink(parentID)
	h.waitForDelivery(link.ChildID)

	// Re-engage the finished child with follow-up work.
	require.NoError(t, h.mgr.SendToChild(h.ctx, link.ChildID, "MORE_WORK for the CHILD_TASK"))

	// A new completion is owed and re-delivered.
	h.waitForDelivery(link.ChildID)

	msgs := h.parentMessages(parentID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.GreaterOrEqual(
		t,
		countSubagentEvents(msgs, link.ChildID),
		2,
		"follow-up produces a second completion record",
	)
}

func TestIntegration_SweepRedeliversIdempotently(t *testing.T) {
	h := newSubagentHarness(t)
	defer h.shutdown()

	ctx := h.ctx

	// Seed a parent + a child that finished but whose completion was never
	// delivered (delivered_at IS NULL) — i.e. the daemon died mid-delivery.
	parent, err := h.sessStore.CreateSession(ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)

	childID, err := h.sessStore.CreateSubagentSession(
		ctx,
		h.projectID,
		parent.ID,
		parent.ID,
		"general",
		"fake-model",
		"",
	)
	require.NoError(t, err)

	require.NoError(t, h.links.InsertSubagentLink(ctx, subagent.Link{
		ParentID: parent.ID, ChildID: childID, TaskCallID: "orphan-call",
	}))
	// Child wrote its final message before dying; its result was stored on the link
	// at terminalization (as a real run would), so delivery reads the column.
	_, err = h.sessStore.InsertMessage(ctx, childID, &transcript.Message{
		Role: llmwire.RoleAssistant, Content: "child finished: 99",
	})
	require.NoError(t, err)
	require.NoError(t, h.links.MarkLinkTerminal(
		ctx, childID, subagent.StateCompleted, "child finished: 99", subagent.OutcomeCompleted,
	))
	require.NoError(t, h.sessStore.UpdateSessionStatus(ctx, childID, sessionstore.SessionStatusCompleted))

	// First sweep delivers exactly one completion.
	h.mgr.sweep(ctx)
	h.waitForDelivery(childID)
	h.mgr.waitIdle(parent.ID)

	msgs := h.parentMessages(parent.ID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Equal(t, 1, countSubagentEvents(msgs, childID), "exactly one record after first sweep")

	// Second sweep is idempotent: the link is now delivered (delivered_at set by the
	// atomic CAS), so it is excluded from the undelivered set and re-injects nothing
	// — still exactly one record, never zero.
	h.mgr.sweep(ctx)
	h.mgr.waitIdle(parent.ID)

	msgs = h.parentMessages(parent.ID)
	assert.Equal(t, 1, countSubagentEvents(msgs, childID), "still exactly one record after second sweep (never zero)")

	// The delivered completion reflects the stored result + outcome.
	require.Contains(t, lastToolResultContent(msgs, "subagent_event"), "completed")
}

// newMCPHarness is newSubagentHarnessWith plus the MCP wiring cmd/coagent does:
// one registry store and one real pool, shared by the daemon's registry tools and
// every session's tool stack.
func newMCPHarness(
	t *testing.T,
	respond func(system string, msgs []llmwire.Message) *llmwire.Response,
) (*subagentHarness, mcpstore.Store, mcp.Pool) {
	return newMCPHarnessWithIdleTTL(t, respond, 0)
}

// newMCPHarnessWithIdleTTL is newMCPHarness with a pool whose live-client idle
// TTL is injected, so scenario tests can exercise idle reaping without waiting
// the production 30 minutes. Zero keeps the default.
func newMCPHarnessWithIdleTTL(
	t *testing.T,
	respond func(system string, msgs []llmwire.Message) *llmwire.Response,
	idleTTL time.Duration,
) (*subagentHarness, mcpstore.Store, mcp.Pool) {
	t.Helper()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")

	db, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(ctx, db, dbPath))

	store := NewStore(db)
	sessStore := sessionstore.NewStore(db)
	links := subagent.NewStore(db)
	schedStore := schedule.NewStore(db)
	registry := mcpstore.NewStore(db)

	var pool mcp.Pool
	if idleTTL > 0 {
		pool = mcp.NewPoolWithIdleTTL(nil, idleTTL)
	} else {
		pool = mcp.NewPool(nil)
	}

	t.Cleanup(pool.Stop)

	workDir := t.TempDir()
	cfg := &config.Config{WorkDir: workDir, Model: "fake-model"}

	factory := session.NewFactoryWithOptions(
		cfg, nil, nil, sessStore, sessStore, nil, pool, registry, nil, nil,
		session.WithLLMClientFactory(func(_ *config.Config) (llm.Client, error) {
			return &scriptedLLM{respond: respond}, nil
		}),
	)

	mgr := newSvc(
		factory,
		store,
		sessStore,
		sessStore,
		sessStore,
		sessStore,
		sessStore,
		sessStore,
		sessStore,
		links,
		subagent.NewTransactions(db),
		budget.New(sessStore),
		sessStore,
		schedule.NewService(schedStore),
		func() string { return "fake-model" },
	)
	mgr.mcpStore = registry
	mgr.mcpPool = pool

	pid, err := store.GetOrCreateProject(ctx, workDir)
	require.NoError(t, err)

	return &subagentHarness{
		t: t, db: db, mgr: mgr, sessStore: sessStore, links: links, schedStore: schedStore,
		projectID: pid, ctx: ctx,
	}, registry, pool
}

// waitIdle blocks until the session has no live runner (best-effort settle).
func (s *svc) waitIdle(sessionID int64) {
	for range 200 {
		if !s.HasActiveLoop(sessionID) {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}
}
