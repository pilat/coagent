package daemon

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/tool"
)

const (
	backgroundExitNudgeMarker = "background processes you started"
	completionNudgeMarker     = "You ended your previous response without calling a tool"
	childBackgroundMarker     = "CHILD_BG"
	backgroundProcessPrefix   = "Background process ID (not an operating-system PID): "
)

// backgroundChildScript reproduces the 2026-10-12 incident: a research child
// backgrounds a slow search and answers before it finishes.
type backgroundChildScript struct {
	release    string
	background bool
	cancel     bool
	// beforeNudgeReply runs when the child first answers the background-exit nudge.
	beforeNudgeReply func()
	nudgeOnce        atomic.Bool
	processID        atomic.Value
	childCalls       atomic.Int64
}

func newBackgroundChildScript(t *testing.T) *backgroundChildScript {
	t.Helper()
	return &backgroundChildScript{release: filepath.Join(t.TempDir(), "release")}
}

func (s *backgroundChildScript) respond(_ string, messages []llmwire.Message) *llmwire.Response {
	if hasUserContaining(messages, childBackgroundMarker) {
		s.childCalls.Add(1)
		return s.child(messages)
	}
	switch {
	case hasUserContaining(messages, "<subagent_completion>"):
		return textReply("parent final")
	case hasToolResultFor(messages, tool.IDTask) && s.background:
		return textReply("parent waiting")
	case hasToolResultFor(messages, tool.IDTask):
		return textReply("parent final")
	}
	args := `{"prompt":"` + childBackgroundMarker + ` research","description":"research","subagent_type":"general"}`
	if s.background {
		args = `{"prompt":"` + childBackgroundMarker + ` research","description":"research","subagent_type":"general","background":true}`
	}
	return callReply(taskCallID, tool.IDTask, args)
}

func (s *backgroundChildScript) child(messages []llmwire.Message) *llmwire.Response {
	nudged := hasUserContaining(messages, backgroundExitNudgeMarker)
	if nudged && s.beforeNudgeReply != nil && s.nudgeOnce.CompareAndSwap(false, true) {
		s.beforeNudgeReply()
	}
	switch {
	case hasUserContaining(messages, "<process_completion>") &&
		!hasUserContaining(messages, "it replaces everything you said before"):
		// The incident's delta-shaped answer, which the nudge must rule out.
		return textReply("background search confirmed the report")
	case hasUserContaining(messages, "<process_completion>"):
		return textReply("final report with search")
	case hasToolResultFor(messages, tool.IDCancelProcess):
		return textReply("final report without search")
	case nudged && s.cancel:
		id, _ := s.processID.Load().(string)
		return callReply("cancel-search", tool.IDCancelProcess, `{"process_id":"`+id+`"}`)
	case nudged:
		return textReply("waiting for the search")
	case hasToolResultFor(messages, "bash"):
		s.processID.Store(backgroundProcessID(messages))
		return textReply("interim report")
	}
	command := `while [ ! -e '` + s.release + `' ]; do sleep 0.05; done; echo searched`
	return callReply("search", "bash", `{"command":"`+command+`","background":true}`)
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

func (s *backgroundChildScript) releaseSearch(t *testing.T) {
	t.Helper()
	require.NoError(t, os.WriteFile(s.release, nil, 0o600))
}

func (h *harness) parentDeliveries(parentID, childID int64) int {
	h.t.Helper()
	var count int
	require.NoError(h.t, h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM session_inbox
		WHERE session_id = ? AND ((source = 'call_result' AND json_extract(attributes, '$.tool_id') = 'task')
			OR (source = 'subagent' AND json_extract(attributes, '$.child_id') = ?))`,
		parentID, childID).Scan(&count))
	return count
}

func (h *harness) waitChildYield(parentID int64) *subagent.Link {
	h.t.Helper()
	h.waitUntil("child link", func() bool { return h.linkByCall(parentID, taskCallID) != nil })
	childID := h.linkByCall(parentID, taskCallID).ChildID
	h.waitUntil("child yield", func() bool {
		return lastAssistantTextDTO(h.messages(childID)) == "waiting for the search" && !h.mgr.HasActiveLoop(childID)
	})
	return h.link(childID)
}

func assertOneChildResult(t *testing.T, h *harness, parentID int64, link *subagent.Link, want string) {
	t.Helper()
	final := h.link(link.ChildID)
	require.NotNil(t, final)
	assert.Equal(t, int64(1), final.ActivationSeq, "the background wait stays inside the first activation")
	assert.Equal(t, subagent.OutcomeCompleted, final.Outcome)
	assert.Equal(t, want, final.Result)
	assert.Equal(t, 1, h.parentDeliveries(parentID, link.ChildID), "one assignment produces one parent result")
	parent := h.messages(parentID)
	require.NoError(t, llm.ValidateToolPairing(parent))
	for _, message := range parent {
		assert.NotContains(t, message.Content, "interim report", "a yield is never delivered as a result")
	}
	child := h.messages(link.ChildID)
	assert.Equal(t, 1, countUserCompletions(child, backgroundExitNudgeMarker))
	assert.True(t, hasUserContaining(child, "it replaces everything you said before"),
		"the nudge tells a waiting child its next answer must be the complete result")
}

// releaseAndAwaitExit lets the search finish and waits for its durable terminal row.
func (s *backgroundChildScript) releaseAndAwaitExit(t *testing.T, h **harness) {
	t.Helper()
	s.releaseSearch(t)
	id, _ := s.processID.Load().(string)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		process, err := (*h).mgr.processStore.GetProcess(context.Background(), id)
		if err == nil && process.State.Terminal() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("process %s did not finish", id)
}

func TestHarnessScenario_ForegroundSubagentBackgroundWorkDeliversOneResult(t *testing.T) {
	script := newBackgroundChildScript(t)
	h := newHarness(t, harnessOptions{respond: script.respond})
	installScenarioProcessService(t, h)
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "research", "fake-model", managerAttrs(scenarioManagerID))
	require.NoError(t, err)

	link := h.waitChildYield(parentID)
	assert.True(t, link.Blocking)
	assert.False(t, link.Terminal(), "the yield leaves the activation open")
	assert.Zero(t, link.DeliveredAt)
	assert.Zero(t, h.parentDeliveries(parentID, link.ChildID), "the parent's task call stays pending")
	assert.Zero(t, countToolResultsFor(h.messages(parentID), tool.IDTask))

	script.releaseSearch(t)
	h.waitUntil("parent answer", func() bool {
		return lastAssistantTextDTO(h.messages(parentID)) == "parent final" && !h.mgr.HasActiveLoop(parentID)
	})
	assertOneChildResult(t, h, parentID, link, "final report with search")
	assert.Contains(t, subagentLastToolResultContent(h.messages(parentID), tool.IDTask), "final report with search")
}

func TestHarnessScenario_BackgroundSubagentBackgroundWorkDeliversOneResult(t *testing.T) {
	script := newBackgroundChildScript(t)
	script.background = true
	h := newHarness(t, harnessOptions{respond: script.respond})
	installScenarioProcessService(t, h)
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "research", "fake-model", managerAttrs(scenarioManagerID))
	require.NoError(t, err)

	link := h.waitChildYield(parentID)
	assert.False(t, link.Blocking)
	assert.Zero(t, link.DeliveredAt)
	h.waitUntil("parent yield", func() bool { return !h.mgr.HasActiveLoop(parentID) })
	assert.Zero(t, h.parentDeliveries(parentID, link.ChildID))

	script.releaseSearch(t)
	h.waitUntil("parent answer", func() bool {
		return lastAssistantTextDTO(h.messages(parentID)) == "parent final" && !h.mgr.HasActiveLoop(parentID)
	})
	assertOneChildResult(t, h, parentID, link, "final report with search")
	parent := h.messages(parentID)
	assert.Equal(t, 1, countUserCompletions(parent, "<subagent_completion>"))
	assert.Equal(t, 1, countUserCompletions(parent, "activation_seq: 1"))
}

func TestHarnessScenario_SubagentCancelsUnneededProcessAfterNudge(t *testing.T) {
	script := newBackgroundChildScript(t)
	script.cancel = true
	h := newHarness(t, harnessOptions{respond: script.respond})
	installScenarioProcessService(t, h)
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "research", "fake-model", managerAttrs(scenarioManagerID))
	require.NoError(t, err)

	h.waitUntil("parent answer", func() bool {
		return lastAssistantTextDTO(h.messages(parentID)) == "parent final" && !h.mgr.HasActiveLoop(parentID)
	})
	link := h.linkByCall(parentID, taskCallID)
	require.NotNil(t, link)
	assertOneChildResult(t, h, parentID, link, "final report without search")
	child := h.messages(link.ChildID)
	assert.Zero(t, countUserCompletions(child, completionNudgeMarker),
		"the background nudge already spent this generation's one host nudge")
	assert.Equal(t, int64(4), script.childCalls.Load(), "launch, stop, cancel, final answer")
	id, _ := script.processID.Load().(string)
	process, err := h.mgr.processStore.GetProcess(h.ctx, id)
	require.NoError(t, err)
	assert.Equal(t, backgroundprocess.StateCancelled, process.State)
}

func TestHarnessScenario_SubagentBackgroundYieldSurvivesRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "subagent-background-restart.db")
	script := newBackgroundChildScript(t)
	first := newHarness(t, harnessOptions{dbPath: dbPath, respond: script.respond})
	installScenarioProcessService(t, first)
	parentID, err := first.mgr.Send(
		first.ctx,
		first.projectID,
		"research",
		"fake-model",
		managerAttrs(scenarioManagerID),
	)
	require.NoError(t, err)
	link := first.waitChildYield(parentID)
	first.shutdown()

	id, _ := script.processID.Load().(string)
	process, err := first.mgr.processStore.GetProcess(first.ctx, id)
	require.NoError(t, err)
	assert.Equal(t, backgroundprocess.StateInterrupted, process.State)
	assert.Zero(t, first.parentDeliveries(parentID, link.ChildID))

	second := newHarness(t, harnessOptions{dbPath: dbPath, respond: script.respond})
	require.NoError(t, second.mgr.Start(second.ctx))
	second.waitUntil("parent answer", func() bool {
		return lastAssistantTextDTO(second.messages(parentID)) == "parent final" && !second.mgr.HasActiveLoop(parentID)
	})
	assertOneChildResult(t, second, parentID, link, "final report with search")
	assert.True(t, hasUserContaining(second.messages(link.ChildID), "state: interrupted"))
}

func TestHarnessScenario_RootBackgroundYieldIsUnchanged(t *testing.T) {
	release := filepath.Join(t.TempDir(), "release")
	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		switch {
		case hasUserContaining(messages, "<process_completion>"):
			return textReply("root done")
		case hasToolResultFor(messages, "bash"):
			return textReply("root waiting")
		}
		command := `while [ ! -e '` + release + `' ]; do sleep 0.05; done; echo built`
		return callReply("build", "bash", `{"command":"`+command+`","background":true}`)
	}
	h := newHarness(t, harnessOptions{respond: respond})
	installScenarioProcessService(t, h)
	rootID, err := h.mgr.Send(h.ctx, h.projectID, "build", "fake-model", managerAttrs(scenarioManagerID))
	require.NoError(t, err)

	h.waitUntil("root yield", func() bool {
		return lastAssistantTextDTO(h.messages(rootID)) == "root waiting" && !h.mgr.HasActiveLoop(rootID)
	})
	assert.Zero(t, countUserCompletions(h.messages(rootID), backgroundExitNudgeMarker), "roots get no exit nudge")
	yielded := slices.ContainsFunc(h.outbox(rootID), func(row outboxRow) bool {
		return strings.HasPrefix(row.Content, "🟣 Background\n\n") && strings.Contains(row.Content, "root waiting")
	})
	assert.True(t, yielded, "the root still publishes its background yield")

	require.NoError(t, os.WriteFile(release, nil, 0o600))
	h.waitUntil("root answer", func() bool {
		return lastAssistantTextDTO(h.messages(rootID)) == "root done" && !h.mgr.HasActiveLoop(rootID)
	})
	assert.Zero(t, countUserCompletions(h.messages(rootID), backgroundExitNudgeMarker))
}
