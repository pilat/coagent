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
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/configapply"
	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/managercontrol"
	"github.com/pilat/coagent/internal/managerdiscovery"
	"github.com/pilat/coagent/internal/mcp"
	"github.com/pilat/coagent/internal/mcpstore"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/progressruntime"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionbuild"
	"github.com/pilat/coagent/internal/sessionbus"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/tool/builtin"
	"github.com/pilat/coagent/internal/transcript"
)

// autoCompactionPromptTokens is what the scripted provider reports as its own
// measurement. It sits above 0.85 of the harness client's 200k window, so the
// automatic threshold fires on a transcript still small enough to summarize.
const autoCompactionPromptTokens = 190000

const autoCompactionBrief = "## Goal\nfinish the scripted task\n" +
	"## Progress\n- ran one tool\n" +
	"## Context for Continuation\nkeep going"

const compactionStartNotice = "🔄 Compacting context..."

const compactionDoneNotice = "✅ Context compacted"

// measuredResponse is a scripted turn that reports a provider measurement over
// the compaction threshold — the only thing that arms the automatic path.
func measuredResponse(resp *llmwire.Response) *llmwire.Response {
	resp.Usage = &llmwire.MessageUsage{PromptTokens: autoCompactionPromptTokens}

	return resp
}

var (
	scriptedCallMu      sync.Mutex
	scriptedCallCounter int
)

// scriptedToolCall mints one uniquely identified scripted tool call: real
// providers never reuse call ids across assistant turns, and pairing validity
// treats reuse as history corruption.
func scriptedToolCall(name, args string) *llmwire.Response {
	scriptedCallMu.Lock()
	defer scriptedCallMu.Unlock()

	scriptedCallCounter++

	return measuredResponse(&llmwire.Response{
		ToolCalls: []llmwire.ToolCall{{
			ID:        fmt.Sprintf("%s-call-%d", name, scriptedCallCounter),
			Name:      name,
			Arguments: []byte(args),
		}},
	})
}

// isCompactionPrompt recognises the summarization call: the replayed prefix
// plus one final checkpoint instruction.
func isCompactionPrompt(msgs []llmwire.Message) bool {
	return isCompactionInstruction(msgs)
}

func indexOfSummary(msgs []llmwire.Message) int {
	for i, m := range msgs {
		if strings.HasPrefix(m.Content, contextSummaryPrefix) {
			return i
		}
	}

	return -1
}

func countSummaryRows(msgs []llmwire.Message) int {
	count := 0

	for _, m := range msgs {
		if strings.HasPrefix(m.Content, contextSummaryPrefix) {
			count++
		}
	}

	return count
}

func indexOfSubagentCompletion(msgs []llmwire.Message) int {
	for i, m := range msgs {
		if m.Role == llmwire.RoleUser && strings.Contains(m.Content, "<subagent_completion>") {
			return i
		}
	}

	return -1
}

func (h *subagentHarness) waitUntil(label string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		if cond() {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	h.t.Fatalf("timed out waiting for: %s", label)
}

var errBudgetParkProbe = errors.New("budget park probe")

type budgetServiceProbe struct {
	budget.Service
	record       *budget.Record
	getErr       error
	releaseErr   error
	releaseCalls int
	beginCalls   chan struct{}
}

func (s *budgetServiceProbe) Get(context.Context, int64) (*budget.Record, error) {
	return s.record, s.getErr
}

func (s *budgetServiceProbe) Release(
	context.Context,
	int64,
	int64,
	string,
) (*budget.Record, error) {
	s.releaseCalls++

	return s.record, s.releaseErr
}

func (s *budgetServiceProbe) BeginDrain(
	context.Context,
	int64,
	int64,
	string,
) (*budget.Record, error) {
	s.beginCalls <- struct{}{}

	return nil, errBudgetParkProbe
}

// The controller-visible compaction vocabulary, duplicated from internal/session
// on purpose: these strings are the contract with the human, not an implementation
// detail, so a reword must break a test outside the package that emits them.
const (
	noticeCompacting       = "🔄 Compacting context..."
	noticeCompacted        = "✅ Context compacted"
	noticeCompactionFailed = "❌ Compaction failed"
	noticeNothingToCompact = "Nothing to compact"
	noticeCompactDeferred  = "⏳ Compaction deferred until the session finishes waiting"
)

func compactionNotices(events []controllerapi.SessionNotification, sessionID int64) []string {
	vocabulary := map[string]bool{
		noticeCompacting:       true,
		noticeCompacted:        true,
		noticeCompactionFailed: true,
		noticeNothingToCompact: true,
		noticeCompactDeferred:  true,
	}

	var out []string

	for _, event := range events {
		if event.SessionID != sessionID || event.Notification.Type != sessionevent.NotifyMessage {
			continue
		}

		if vocabulary[event.Notification.Message] {
			out = append(out, event.Notification.Message)
		}
	}

	return out
}

// compactOnlyRespond answers summarization prompts with a brief, does one
// unmeasured tool round before settling (the verbatim tail is never empty, so
// a compactable transcript needs two raw groups), and answers everything else
// with plain text.
func compactOnlyRespond(_ string, msgs []llmwire.Message) *llmwire.Response {
	if isCompactionInstruction(msgs) {
		return &llmwire.Response{
			Text: "## Goal\nsome work\n## Progress\n- done\n## Context for Continuation\ncarry on",
		}
	}

	if hasToolResultFor(msgs, "ls") {
		return &llmwire.Response{Text: "work done"}
	}

	return &llmwire.Response{
		ToolCalls: []llmwire.ToolCall{{
			ID:        "ls-1",
			Name:      "ls",
			Arguments: []byte(`{"path":"."}`),
		}},
	}
}

const contextSummaryPrefix = "[CONTEXT SUMMARY"

// isCompactionInstruction recognises the summarization call: a multi-message
// request whose final message is the checkpoint instruction, not an ordinary
// user turn.
func isCompactionInstruction(msgs []llmwire.Message) bool {
	if len(msgs) == 0 {
		return false
	}

	last := msgs[len(msgs)-1]

	return strings.Contains(last.Content, "continuation checkpoint")
}

// blockingCompactRespond drives a parent that settles one tool round, spawns
// one blocking child (the newest group — it stays verbatim in the tail), and
// answers with a compaction brief whenever it is handed a summarization prompt.
// The extra round exists because the verbatim tail is never empty: a deferred
// /compact needs a summarizable group besides the launch pair.
func blockingCompactRespond(release <-chan struct{}) func(string, []llmwire.Message) *llmwire.Response {
	return func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if isCompactionInstruction(msgs) {
			return &llmwire.Response{
				Text: "## Goal\nspawn work\n## Progress\n- child ran\n## Context for Continuation\ncarry on",
			}
		}

		if hasUserContaining(msgs, "CHILD_TASK") {
			if release != nil {
				<-release
			}

			return &llmwire.Response{Text: "blocking child done: 7"}
		}

		// The deferred /compact continues the activation, so the parent may be
		// asked again over the compacted transcript — answer, don't re-spawn.
		if hasSummaryRow(msgs) || hasToolResultFor(msgs, "task") {
			return &llmwire.Response{Text: "parent got the child result"}
		}

		if !hasToolResultFor(msgs, "ls") {
			return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
				ID:        "ls-before-spawn",
				Name:      "ls",
				Arguments: []byte(`{"path":"."}`),
			}}}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID:        taskCallID,
			Name:      "task",
			Arguments: []byte(`{"prompt":"CHILD_TASK do it","description":"c","subagent_type":"general"}`),
		}}}
	}
}

func hasSummaryRow(msgs []llmwire.Message) bool {
	for _, m := range msgs {
		if strings.HasPrefix(m.Content, contextSummaryPrefix) {
			return true
		}
	}

	return false
}

// ledgerHarness is a live daemon whose link store fails on demand, plus the ids
// of a parent and one non-terminal child of it.
type ledgerHarness struct {
	*subagentHarness

	flaky      *flakyLinkStore
	activation *flakyActivationStore
	parentID   int64
	childID    int64
}

type rejectingCompletionTransactions struct {
	subagent.Store
}

func (rejectingCompletionTransactions) DeliverCompletion(
	context.Context,
	subagent.Link,
	string,
) (bool, error) {
	return false, sessionstore.ErrSessionNotAcceptingInput
}

func newLedgerHarness(t *testing.T) *ledgerHarness {
	t.Helper()

	var flaky *flakyLinkStore

	h := newSubagentHarnessDecorated(t, trivialRespond, func(inner subagent.Store) subagent.Store {
		flaky = newFlakyLinkStore(inner)
		return flaky
	})
	activation := &flakyActivationStore{Store: h.mgr.links}
	h.mgr.links = activation

	parent, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)

	childID, err := func() (int64, error) {
		var id int64
		err := h.sessStore.WithTx(h.ctx, func(tx *sql.Tx) error {
			var err error
			id, err = sessionstore.CreateSubagentSessionTx(
				h.ctx,
				tx,
				sessionstore.CreateSubagentSession{
					ProjectID:      h.projectID,
					ParentID:       parent.ID,
					RootID:         parent.ID,
					AgentType:      "general",
					Model:          "fake-model",
					ReasoningLevel: "",
				},
			)
			return err
		})
		return id, err
	}()
	require.NoError(t, err)
	require.NoError(t, seedChildLink(h.ctx, h.sessStore, subagent.Link{
		ParentID: parent.ID, ChildID: childID, TaskCallID: "bg",
	}))

	return &ledgerHarness{
		subagentHarness: h, flaky: flaky, activation: activation,
		parentID: parent.ID, childID: childID,
	}
}

// drainNotifications collects everything buffered on a per-session subscription.
func drainNotifications(ch <-chan sessionevent.Notification) []sessionevent.Notification {
	var out []sessionevent.Notification

	for {
		select {
		case n := <-ch:
			out = append(out, n)
		case <-time.After(200 * time.Millisecond):
			return out
		}
	}
}

// trivialRespond: every session finishes immediately. Used when the test drives
// Spawn directly and does not want children to spawn further.
func trivialRespond(_ string, _ []llmwire.Message) *llmwire.Response {
	return &llmwire.Response{Text: "done"}
}

// blockingParentRespond builds a respond that spawns one blocking child, then
// finishes once the child's result lands. childBody runs as the child's turn.
func blockingParentRespond(childBody func() *llmwire.Response) func(string, []llmwire.Message) *llmwire.Response {
	return func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "CHILD_TASK") {
			return childBody()
		}

		if hasToolResultFor(msgs, "task") {
			return &llmwire.Response{Text: "parent done"}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID:        taskCallID,
			Name:      "task",
			Arguments: []byte(`{"prompt":"CHILD_TASK","description":"c","subagent_type":"general"}`),
		}}}
	}
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

func (h *subagentHarness) queueLen() int {
	return runnerWaitingCount(h.mgr.runners)
}

func (h *subagentHarness) waitForLinkByCall(parentID int64, callID string) subagent.Link {
	h.t.Helper()
	h.waitUntil("link for "+callID, func() bool {
		link, err := h.links.GetLinkByTaskCallID(h.ctx, parentID, callID)
		return err == nil && link != nil
	})

	link, err := h.links.GetLinkByTaskCallID(h.ctx, parentID, callID)
	require.NoError(h.t, err)

	return *link
}

const (
	applyCallA = "cfg-call-a"
	applyCallB = "cfg-call-b"
)

// twoSessionApplyRespond drives two root sessions that both reach for the same
// config knob after their /config grant lands. The opener prompt tells the
// sessions apart and the call ids differ, so a call left unanswered is visible
// in the transcript it belongs to.
func twoSessionApplyRespond(_ string, msgs []llmwire.Message) *llmwire.Response {
	if hasToolResultFor(msgs, tool.IDConfigEdit) {
		return &llmwire.Response{Text: "configuration replaced"}
	}

	if !hasUserContaining(msgs, configapply.ConfigEditCommand) {
		return &llmwire.Response{Text: "ready to reconfigure"}
	}

	if hasUserContaining(msgs, "APPLY_B") {
		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID:        applyCallB,
			Name:      tool.IDConfigEdit,
			Arguments: json.RawMessage(`{"document":` + mustQuoteJSON(configEditCandidateB) + `}`),
		}}}
	}

	return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
		ID:        applyCallA,
		Name:      tool.IDConfigEdit,
		Arguments: json.RawMessage(`{"document":` + mustQuoteJSON(configEditCandidateA) + `}`),
	}}}
}

const configEditCandidateA = `providers:
    work:
        driver: anthropic
        api_key: ${WORK_API_KEY}
models:
    - id: claude-opus-5
      provider: work
    - id: claude-sonnet-5
      provider: work
`

const configEditCandidateB = `providers:
    work:
        driver: anthropic
        api_key: ${WORK_API_KEY}
models:
    - id: claude-sonnet-5
      provider: work
    - id: claude-opus-5
      provider: work
`

func newApplyDaemonWith(
	t *testing.T,
	dbPath, configDir string,
	respond func(system string, msgs []llmwire.Message) *llmwire.Response,
) *applyDaemon {
	t.Helper()

	h := newSubagentHarnessOnDB(t, dbPath, respond, nil)
	ops := configops.New(filepath.Join(configDir, "config.yaml"), filepath.Join(configDir, "secrets"))

	h.mgr.applier = configapply.New(ops, h.sessStore)

	return &applyDaemon{subagentHarness: h, ops: ops, restarts: h.mgr.applier.Restart()}
}

func defaultModelInFile(t *testing.T, configDir string) string {
	t.Helper()

	body, err := os.ReadFile(filepath.Join(configDir, "config.yaml"))
	require.NoError(t, err)

	opus := strings.Index(string(body), "id: claude-opus-5")
	sonnet := strings.Index(string(body), "id: claude-sonnet-5")
	require.NotEqual(t, -1, opus)
	require.NotEqual(t, -1, sonnet)

	if opus < sonnet {
		return "claude-opus-5"
	}

	return "claude-sonnet-5"
}

const applyCallID = "cfg-call-1"

// applyDaemon is one daemon "process image" over a shared database and a shared
// config directory, so a test can take one down and bring the next one up on the
// same durable state the way a restart-apply does.
type applyDaemon struct {
	*subagentHarness
	ops      configops.Service
	restarts <-chan struct{}
}

// configApplyRespond calls config_edit once the /config grant exists, then
// answers once its verdict is in the transcript. A second call would mean the
// suspended call was re-executed.
func configApplyRespond(_ string, msgs []llmwire.Message) *llmwire.Response {
	if hasToolResultFor(msgs, tool.IDConfigEdit) {
		return &llmwire.Response{Text: "configuration replaced"}
	}

	if hasUserContaining(msgs, configapply.ConfigEditCommand) {
		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID:        applyCallID,
			Name:      tool.IDConfigEdit,
			Arguments: json.RawMessage(`{"document":` + mustQuoteJSON(configEditCandidate) + `}`),
		}}}
	}

	return &llmwire.Response{Text: "ready to reconfigure"}
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

func newApplyDaemon(t *testing.T, dbPath, configDir string) *applyDaemon {
	t.Helper()

	return newApplyDaemonWith(t, dbPath, configDir, configApplyRespond)
}

func (d *applyDaemon) restartCount() int { return len(d.restarts) }

func (d *applyDaemon) waitForConfigResult(sessionID int64) {
	d.waitUntil("config verdict consumed", func() bool {
		return hasToolResultFor(d.parentMessages(sessionID), tool.IDConfigEdit) && !d.mgr.HasActiveLoop(sessionID)
	})
}

func (d *applyDaemon) waitForRestart(t *testing.T) {
	t.Helper()

	select {
	case <-d.restarts:
	case <-time.After(5 * time.Second):
		t.Fatal("the staged config change never asked for a restart")
	}
}

// bootVerdict replays what cmd/coagent's boot does with a marker: resolve it,
// spend the grant behind an applied verdict, then hand the verdict to the
// session that suspended.
func (d *applyDaemon) bootVerdict(t *testing.T) (configops.Outcome, error) {
	t.Helper()

	pending, err := d.ops.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, pending, "the boot after an apply must still find the marker")

	outcome, err := d.ops.ResolvePending(*pending, nil)
	require.NoError(t, err)

	if !outcome.Verdict.Failed() {
		d.mgr.applier.ConsumeConfigEditActivation(
			d.ctx, outcome.Pending.SessionID, outcome.Pending.ToolCallID,
		)
	}

	message := "Config applied: " + outcome.Pending.Summary
	if outcome.Verdict.Failed() {
		message = "Config change rejected — " + outcome.Verdict.Reason()
	}

	if _, err := enqueueCallResult(

		d.ctx, d.mgr.store,

		outcome.Pending.SessionID,
		outcome.Pending.ToolCallID,
		outcome.Pending.ToolName,
		message,
	); err != nil {
		return outcome, err
	}

	return outcome, d.ops.ClearPending(outcome.Pending)
}

// stageApplyAndStop runs a session up to the suspend the config tool causes,
// commits the change, and takes the daemon down the way the restart would.
func stageApplyAndStop(t *testing.T, dbPath, configDir string) int64 {
	t.Helper()

	first := newApplyDaemon(t, dbPath, configDir)

	first.startInboxWake()
	sessionID, err := first.mgr.Send(
		first.ctx, first.projectID, "reconfigure the daemon", "fake-model",
		map[string]any{"manager_id": "telegram:main"},
	)
	require.NoError(t, err)
	first.waitUntil("opener turn settled", func() bool {
		return !first.mgr.HasActiveLoop(sessionID)
	})
	first.mgr.waitIdle(sessionID)

	first.startInboxWake()
	require.NoError(t, first.mgr.sendToSession(first.ctx, sessionID, configapply.ConfigEditCommand))

	first.waitForRestart(t)
	first.waitUntil("session suspended on the config call", func() bool {
		return !first.mgr.HasActiveLoop(sessionID)
	})

	pending, err := first.ops.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, pending, "the commit leaves a marker naming the waiting session")
	require.Equal(t, sessionID, pending.SessionID)
	require.Equal(t, applyCallID, pending.ToolCallID)
	require.Equal(t, tool.IDConfigEdit, pending.ToolName)

	msgs := first.parentMessages(sessionID)
	require.Equal(t, 1, countAssistantToolCallsFor(msgs, tool.IDConfigEdit))
	require.Zero(t, countToolResultsFor(msgs, tool.IDConfigEdit), "the call is out with the world")

	first.shutdown()

	return sessionID
}

// failingCommitOps is an ops layer whose write fails, so the apply is rejected
// with no restart and no marker — the verdict has to come back inline.
type failingCommitOps struct {
	configops.Service
}

func (failingCommitOps) Commit(*configops.Staged, configops.Pending) configops.Verdict {
	return configops.Reject("", errors.New("no space left on device"))
}

// configEditCallID is the call id the responder always uses, so the
// redelivery test can replay the exact same verdict.
const configEditCallID = "cfg-edit-call-1"

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

func testAbandonedConfigCommand(t *testing.T, restart bool) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "abandoned-command.db")
	configDir := newApplyConfigDir(t)
	d := newApplyDaemonWith(t, dbPath, configDir, configEditRespond)
	defer func() { d.shutdown() }()
	d.startInboxWake()
	id, err := d.mgr.Send(d.ctx, d.projectID, "hello", "fake-model", map[string]any{"manager_id": "telegram:main"})
	require.NoError(t, err)
	d.mgr.waitIdle(id)
	_, err = d.db.ExecContext(
		d.ctx,
		`CREATE TRIGGER reject_config_suspend BEFORE UPDATE OF status ON sessions WHEN NEW.status = 'suspended' BEGIN SELECT RAISE(ABORT, 'injected suspend failure'); END`,
	)
	require.NoError(t, err)
	if restart {
		_, err = d.db.ExecContext(d.ctx, `CREATE TRIGGER reject_apply_result BEFORE INSERT ON messages
			WHEN NEW.role = 'tool' AND NEW.tool_name = 'config_edit'
			BEGIN SELECT RAISE(ABORT, 'injected result write failure'); END`)
		require.NoError(t, err)
	}
	require.NoError(t, d.mgr.sendToSession(d.ctx, id, "/config change the default model"))
	d.mgr.waitIdle(id)
	d.waitUntil(
		"abandoned config activation expired",
		func() bool { return currentActivationOf(t, d.subagentHarness, id) == nil },
	)
	if !restart {
		d.waitUntil(
			"abandoned config result settled",
			func() bool { return hasToolResultFor(d.parentMessages(id), tool.IDConfigEdit) },
		)
	}

	activation := currentActivationOf(t, d.subagentHarness, id)
	assert.Nil(t, activation)
	if restart {
		assert.False(t, hasToolResultFor(d.parentMessages(id), tool.IDConfigEdit))
		_, err = d.db.ExecContext(d.ctx, "DROP TRIGGER reject_apply_result")
		require.NoError(t, err)
		_, err = d.db.ExecContext(d.ctx, "DROP TRIGGER reject_config_suspend")
		require.NoError(t, err)
		d.shutdown()
		d = newApplyDaemonWith(t, dbPath, configDir, configEditRespond)
		require.NoError(t, d.mgr.Start(d.ctx))
		d.mgr.waitIdle(id)
	} else {
		_, err = d.db.ExecContext(d.ctx, "DROP TRIGGER reject_config_suspend")
		require.NoError(t, err)
		assert.Contains(t, lastToolResultContent(d.parentMessages(id), tool.IDConfigEdit), "Config change abandoned")
	}
	require.NoError(t, d.mgr.sendToSession(d.ctx, id, "continue after cancellation"))
	d.mgr.waitIdle(id)
	assert.True(t, hasUserContaining(d.parentMessages(id), "continue after cancellation"))
	assert.Equal(t, 1, countToolResultsFor(d.parentMessages(id), tool.IDConfigEdit))
	assert.Zero(t, d.restartCount())
}

func testAbandonedApply(t *testing.T, failure string) {
	t.Helper()
	d := newApplyDaemonWith(t, filepath.Join(t.TempDir(), "abandoned.db"), newApplyConfigDir(t),
		func(string, []llmwire.Message) *llmwire.Response { return &llmwire.Response{Text: "ready"} })
	defer d.shutdown()
	d.startInboxWake()
	id, err := d.mgr.Send(d.ctx, d.projectID, "hello", "fake-model", nil)
	require.NoError(t, err)
	d.mgr.waitIdle(id)
	calls, err := json.Marshal([]llmwire.ToolCall{{ID: configEditCallID, Name: tool.IDConfigEdit}})
	require.NoError(t, err)
	_, err = d.sessStore.Commit(d.ctx, sessionstore.Commit{SessionID: id, Messages: []*transcript.Message{{
		Role: llmwire.RoleAssistant, ToolCalls: calls,
	}}})
	require.NoError(t, err)
	_, err = configapply.NewConfigEdit(id, d.mgr.applier).Execute(
		grantedCall(d.ctx, id, configEditCallID), configEditArgs(configEditCandidate),
	)
	require.ErrorIs(t, err, tool.ErrSuspend)
	configuredModels := d.mgr.build.Config.UnifiedConfig.Models
	d.mgr.build.Config.UnifiedConfig.Models = nil
	failWrite := failure == "write failure"
	if failWrite {
		_, err = d.db.ExecContext(d.ctx, `CREATE TRIGGER reject_apply_result BEFORE INSERT ON messages
			WHEN NEW.role = 'tool' AND NEW.tool_name = 'config_edit'
			BEGIN SELECT RAISE(ABORT, 'injected result write failure'); END`)
		require.NoError(t, err)
	}
	require.NoError(t, d.mgr.sendToSession(d.ctx, id, "keep this input"))
	if failure == "stop" || failure == "kill" {
		if failure == "stop" {
			require.NoError(t, d.mgr.sendToSession(d.ctx, id, "/stop"))
			assert.Contains(t, lastToolResultContent(d.parentMessages(id), tool.IDConfigEdit), "Stopped by user")
		} else {
			require.NoError(t, d.mgr.sendToSession(d.ctx, id, "/kill"))
			assert.False(t, hasToolResultFor(d.parentMessages(id), tool.IDConfigEdit))
		}
		assert.Zero(t, d.restartCount())
		return
	}
	d.mgr.waitIdle(id)
	d.waitUntil("failed construction settled its staged apply", func() bool {
		record, err := d.sessStore.GetSession(d.ctx, id)

		return err == nil && record.Status == sessionstore.SessionStatusError &&
			!d.mgr.HasActiveLoop(id) && (failWrite || !d.mgr.applier.Has(id))
	})

	pending, err := d.sessStore.PeekPending(d.ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "keep this input", pending.RawContent)
	if failWrite {
		assert.True(t, d.mgr.applier.Has(id))
		assert.False(t, hasToolResultFor(d.parentMessages(id), tool.IDConfigEdit))
		_, err = d.db.ExecContext(d.ctx, "DROP TRIGGER reject_apply_result")
		require.NoError(t, err)

		d.mgr.build.Config.UnifiedConfig.Models = configuredModels
		require.NoError(t, d.mgr.sendToSession(d.ctx, id, "retry now"))
		d.mgr.waitIdle(id)
		_, err = d.sessStore.PeekPending(d.ctx, id)
		require.ErrorIs(t, err, sessionstore.ErrNoPendingInput)
		assert.True(t, hasUserContaining(d.parentMessages(id), "keep this input"))
	}
	assert.False(t, d.mgr.applier.Has(id))
	assert.Contains(t, lastToolResultContent(d.parentMessages(id), tool.IDConfigEdit), "Config change abandoned")
	assert.Zero(t, d.restartCount())
	apply, err := d.ops.LoadPending()
	require.NoError(t, err)
	assert.Nil(t, apply)
}

// configEditRespond drives the authorized two-turn script: the opener turn
// answers with text so the session settles before the test sends the real
// "/config" command through SendToSession; the /config turn calls config_edit
// with the full replacement document; after the verdict lands it answers once.
// A second call would mean the suspended call was re-executed.
func configEditRespond(_ string, msgs []llmwire.Message) *llmwire.Response {
	if hasToolResultFor(msgs, tool.IDConfigEdit) {
		return &llmwire.Response{Text: "configuration replaced"}
	}

	if hasUserContaining(msgs, configapply.ConfigEditCommand) {
		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID:        configEditCallID,
			Name:      tool.IDConfigEdit,
			Arguments: json.RawMessage(`{"document":` + mustQuoteJSON(configEditCandidate) + `}`),
		}}}
	}

	return &llmwire.Response{Text: "ready to reconfigure"}
}

// unauthorizedConfigEditRespond calls config_edit with no /config turn, so the
// session must answer the authorization refusal in-process.
func unauthorizedConfigEditRespond(_ string, msgs []llmwire.Message) *llmwire.Response {
	if hasToolResultFor(msgs, tool.IDConfigEdit) {
		return &llmwire.Response{Text: "configuration replaced"}
	}

	return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
		ID:        configEditCallID,
		Name:      tool.IDConfigEdit,
		Arguments: json.RawMessage(`{"document":` + mustQuoteJSON(configEditCandidate) + `}`),
	}}}
}

func mustQuoteJSON(s string) string {
	encoded, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}

	return string(encoded)
}

// startConfigEditSession settles the opener turn, then sends the real /config
// command through the durable input boundary — the same path a manager message
// takes — so the promotion transaction that carries the grant cannot race the
// session's first model call.
func startConfigEditSession(t *testing.T, d *applyDaemon, prompt string) int64 {
	t.Helper()

	d.startInboxWake()
	sessionID, err := d.mgr.Send(
		d.ctx,
		d.projectID,
		prompt,
		"fake-model",
		map[string]any{"manager_id": "telegram:main"},
	)
	require.NoError(t, err)

	d.waitUntil("opener turn settled", func() bool {
		return !d.mgr.HasActiveLoop(sessionID)
	})
	d.mgr.waitIdle(sessionID)

	require.NoError(t, d.mgr.sendToSession(d.ctx, sessionID, configapply.ConfigEditCommand))

	return sessionID
}

// currentActivationOf returns the session's current grant. A missing grant is
// nil; any read failure fails the test instead of passing as absence.
func currentActivationOf(t *testing.T, h *subagentHarness, sessionID int64) *sessionstore.ToolActivation {
	t.Helper()

	activation, err := h.sessStore.
		CurrentActivation(context.Background(), sessionID)
	if errors.Is(err, sessionstore.ErrActivationNotFound) {
		return nil
	}

	require.NoError(t, err)

	return activation
}

func configBytesOf(t *testing.T, configDir string) string {
	t.Helper()

	body, err := os.ReadFile(filepath.Join(configDir, "config.yaml"))
	require.NoError(t, err)

	return string(body)
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

type configHarness struct {
	mgr *svc
	// sessionID is a real session row: the apply pipeline reads its transcript
	// before it commits, so a config tool needs somewhere to have suspended.
	sessionID int64
	projectID int64
	sessions  *sessionstore.Store
	store     Store
	factory   *mockFactory
	tools     map[string]tool.Tool
	restarts  int
	config    string
}

func newConfigHarness(t *testing.T) *configHarness {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	secretsPath := filepath.Join(dir, "secrets")

	require.NoError(t, os.WriteFile(configPath, []byte(toolConfig), 0o600))
	require.NoError(t, os.WriteFile(secretsPath, []byte(toolSecrets), 0o600))

	h := &configHarness{config: configPath}
	mgr, factory, store := newTestManager(t)
	sessions, ok := mgr.store.(*sessionstore.Store)
	require.True(t, ok)
	h.sessions = sessions
	h.store = store
	h.factory = factory
	projectID, err := store.GetOrCreateProject(context.Background(), t.TempDir())
	require.NoError(t, err)
	h.projectID = projectID
	mgr.applier = configapply.New(configops.New(configPath, secretsPath), h.sessions)

	h.mgr = mgr
	h.sessionID = h.liveSession(t)
	h.tools = map[string]tool.Tool{tool.IDConfigEdit: configapply.NewConfigEdit(h.sessionID, mgr.applier)}

	return h
}

// configHarnessCandidate reorders the models, so staging it visibly changes the file.
const configHarnessCandidate = `providers:
    work:
        driver: anthropic
        api_key: ${WORK_API_KEY}
models:
    - id: claude-opus-5
      provider: work
    - id: claude-sonnet-5
      provider: work
`

// grantedCall carries what config_edit demands: the call id plus the durable
// /config grant the tool revalidates before staging.
func grantedCall(ctx context.Context, sessionID int64, callID string) context.Context {
	ctx = tool.WithCallID(ctx, callID)

	return tool.WithActivationGrant(ctx, tool.ActivationGrant{
		SessionID: sessionID, ToolID: tool.IDConfigEdit, Command: "/config", ToolCallID: callID,
	})
}

func configEditArgs(document string) json.RawMessage {
	encoded, err := json.Marshal(document)
	if err != nil {
		panic(err)
	}

	return json.RawMessage(`{"document":` + string(encoded) + `}`)
}

// grantedCall runs config_edit the way the loop does: the assistant turn is
// persisted first, then the tool executes with the grant in context.
func (h *configHarness) grantedCall(t *testing.T, callID, document string) error {
	t.Helper()

	h.recordCall(t, callID, tool.IDConfigEdit)

	_, err := h.tools[tool.IDConfigEdit].Execute(
		grantedCall(context.Background(), h.sessionID, callID), configEditArgs(document),
	)
	if errors.Is(err, tool.ErrSuspend) {
		require.NoError(
			t,
			h.sessions.UpdateSessionStatus(t.Context(), h.sessionID, sessionstore.SessionStatusSuspended),
		)
	}

	return err
}

// liveSession creates a real session record, so notification delivery has
// somewhere to land.
func (h *configHarness) liveSession(t *testing.T) int64 {
	t.Helper()

	ctx := context.Background()
	rec, err := h.mgr.store.CreateSession(
		ctx, h.projectID, "fake-model", "", map[string]any{"channel": "cli", "manager_id": "cli"},
	)
	require.NoError(t, err)

	return rec.ID
}

// recordCall appends the assistant turn a tool_call arrives in, which is what
// makes a later suspend durable.
func (h *configHarness) recordCall(t *testing.T, callID, toolName string) {
	t.Helper()
	activation, activationErr := h.sessions.PendingActivation(t.Context(), h.sessionID)
	if activationErr == nil && !h.mgr.applier.Has(h.sessionID) {
		_, err := h.sessions.Commit(t.Context(), sessionstore.Commit{
			SessionID: h.sessionID,
			Activation: &sessionstore.ActivationChange{
				InputID: activation.InputID,
				State:   sessionstore.ActivationExpired,
			},
		})
		require.NoError(t, err)
	}

	if !h.mgr.applier.Has(h.sessionID) {
		input, err := h.sessions.Enqueue(
			context.Background(),
			sessionstore.Input{SessionID: h.sessionID, Source: sessionstore.InputSourceUser, Content: "/config"},
		)
		require.NoError(t, err)
		_, err = h.sessions.Commit(
			context.Background(),
			sessionstore.Commit{
				SessionID: h.sessionID,
				Accept: []sessionstore.Accept{
					{
						InputID:    input.Input.ID,
						State:      sessionstore.InputStateAccepted,
						Content:    "/config",
						LinkRef:    -1,
						ModelBound: true,
					},
				},
				Activation: &sessionstore.ActivationChange{
					InputID: input.Input.ID,
					State:   sessionstore.ActivationPending,
					ToolID:  toolName,
					Command: "/config",
				},
			},
		)
		require.NoError(t, err)
	}
	calls, err := json.Marshal([]llmwire.ToolCall{{ID: callID, Name: toolName}})
	require.NoError(t, err)

	_, err = h.sessions.Commit(
		context.Background(),
		sessionstore.Commit{SessionID: h.sessionID, Messages: []*transcript.Message{{
			Role:      llmwire.RoleAssistant,
			ToolCalls: calls,
		}}},
	)
	require.NoError(t, err)
}

// restart models the boot a committed apply causes: the new process image comes
// up with a free apply slot and delivers the verdict the call was waiting for.
func (h *configHarness) restart(t *testing.T, callID, toolName string) {
	t.Helper()

	h.restarts += len(h.mgr.applier.Restart())
	h.mgr.applier.ReleaseApply()
	h.mgr.applier = configapply.New(h.mgr.applier.Ops(), h.sessions)
	h.tools[tool.IDConfigEdit] = configapply.NewConfigEdit(h.sessionID, h.mgr.applier)

	_, err := h.sessions.Commit(
		context.Background(),
		sessionstore.Commit{SessionID: h.sessionID, Messages: []*transcript.Message{{
			Role:       llmwire.RoleTool,
			ToolCallID: callID,
			ToolName:   toolName,
			Content:    "Config applied.",
		}}},
	)
	require.NoError(t, err)
}

func (h *configHarness) restartCount() int { return h.restarts + len(h.mgr.applier.Restart()) }

func (h *configHarness) configBytes(t *testing.T) string {
	t.Helper()

	data, err := os.ReadFile(h.config)
	require.NoError(t, err)

	return string(data)
}

type configWireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Name    string `json:"name"`
}

func testConfigDocumentThroughHTTP(t *testing.T, damaged bool) {
	t.Helper()
	initial := strings.ReplaceAll(toolConfig, "claude-sonnet-5", "anthropic/claude-sonnet-4.6")
	initial = strings.ReplaceAll(initial, "claude-opus-5", "anthropic/claude-opus-4.6")
	candidate := initial + "sandbox:\n    rules:\n        - deny: ~/.ssh\n"
	if damaged {
		candidate = strings.ReplaceAll(candidate, "claude-sonnet-4.6", "")
		candidate = strings.ReplaceAll(candidate, "claude-opus-4.6", "")
	}
	configDir := newApplyConfigDirWith(t, initial)
	configPath := filepath.Join(configDir, "config.yaml")
	var sawRead atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []configWireMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		message := map[string]any{"role": "assistant", "content": "ready"}
		finish := "stop"
		command, read, edited := false, false, false
		for _, input := range request.Messages {
			command = command || input.Role == "user" && strings.Contains(input.Content, "/config")
			if input.Role == "tool" && input.Name == "read" {
				read = true
				sawRead.Store(true)
				assert.Contains(t, input.Content, "anthropic/claude-sonnet-4.6")
				assert.Contains(t, input.Content, "anthropic/claude-opus-4.6")
			}
			edited = edited || input.Role == "tool" && input.Name == tool.IDConfigEdit
		}
		if command && !edited {
			name, args := "read", map[string]string{"file_path": configPath}
			if read {
				name, args = tool.IDConfigEdit, map[string]string{"document": candidate}
			}
			encoded, err := json.Marshal(args)
			assert.NoError(t, err)
			message["tool_calls"] = []any{map[string]any{
				"id": name + "-call", "type": "function",
				"function": map[string]any{"name": name, "arguments": string(encoded)},
			}}
			finish = "tool_calls"
		}
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": message, "finish_reason": finish}},
		}))
	}))
	defer server.Close()
	d := newApplyDaemonWith(t, filepath.Join(t.TempDir(), "transport.db"), configDir, configEditRespond)
	defer d.shutdown()
	workDir, err := d.mgr.store.GetProjectWorkDir(d.ctx, d.projectID)
	require.NoError(t, err)
	wireConfig := &config.Config{Model: "fake-model", UnifiedConfig: &config.UnifiedConfig{
		Providers: map[string]config.ProviderEntry{"test": {Driver: "openai", BaseURL: server.URL, APIKey: "test-key"}},
		Models:    []config.ModelEntry{{ID: "fake-model", Provider: "test", MaxTokens: 8192, ContextWindow: 100000}},
	}}
	d.mgr.build.Config = wireConfig
	d.mgr.build.WorkDir = workDir
	id := startConfigEditSession(t, d, "hello")
	d.mgr.waitIdle(id)
	assert.True(t, sawRead.Load())
	var storedDocuments []string
	for _, message := range d.parentMessages(id) {
		for _, call := range message.ToolCalls {
			if call.Name == tool.IDConfigEdit {
				var args struct {
					Document string `json:"document"`
				}
				require.NoError(t, json.Unmarshal(call.Arguments, &args))
				storedDocuments = append(storedDocuments, args.Document)
			}
		}
	}
	assert.Equal(t, []string{candidate}, storedDocuments)
	actual, err := os.ReadFile(configPath)
	require.NoError(t, err)
	if damaged {
		assert.Equal(t, initial, string(actual))
		assert.Zero(t, d.restartCount())
		assert.Contains(t, lastToolResultContent(d.parentMessages(id), tool.IDConfigEdit), "duplicate model id")
		return
	}
	assert.Equal(t, candidate, string(actual))
	assert.Equal(t, 1, d.restartCount())
}

func newTestController(
	svc *svc,
	cfg *config.Config,
	cache loader.MarketplaceCache,
	_ schedule.Service,
) controllerapi.ManagerControllerFactory {
	var outputs *sessionstore.Store
	if svc != nil {
		outputs, _ = svc.store.(*sessionstore.Store)
	}

	return managercontrol.New(
		svc,
		outputs,
		managerdiscovery.New(outputs, cfg, cache),
		svc.progress,
		svc.bus,
		cfg,
		cache,
	)
}

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

	store := sessionstore.NewStore(db)
	sessStore := sessionstore.NewStore(db)
	links := subagent.NewStore(db, sessStore)
	schedStore := schedule.NewStore(db, sessStore)

	workDir := t.TempDir()
	writeProjectAgents(t, workDir, agents)

	rec := &schemaRecorder{names: make(map[int64]map[string]bool)}
	cfg := &config.Config{WorkDir: workDir, Model: "fake-model"}

	factory := scriptedBuildInput(t, cfg, sessStore, nil, func(_ *config.Config) (llm.Client, error) {
		return &recordingLLM{respond: respond, rec: rec}, nil
	})

	mgr, _ := newScenarioDaemon(
		context.Background(),
		factory,
		sessStore,
		links,
		budget.New(sessStore),
		schedule.NewService(schedStore, sessStore),
		func() string { return "fake-model" },
		db,
	)
	mgr.applier = configapply.New(newTestConfigOps(t, dir), sessStore)

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
// files, so config_edit can be registered on root sessions.
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

func (h *gatingHarness) waitForLink(parentID int64, callID string) subagent.Link {
	h.t.Helper()

	var link *subagent.Link

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

// A subagent must never reach the control plane, whatever its allowlist says.
var controlPlaneTools = []string{tool.IDSleep, tool.IDTask, tool.IDSchedule}

var configPlaneTools = []string{tool.IDConfigEdit}

const scoutAgentFile = `---
name: scout
description: Restricted project research subagent
tools:
  - read
  - grep
---
You are the project scout.
`

const wideAgentFile = `---
name: wide
description: Project subagent with an unrestricted tool list
tools:
  - "*"
---
You are the wide project agent.
`

// drainScenarioClaims drains every output the scenario's manager owns through
// the production controller and acknowledges each claim exactly like the real
// manager would. Under -update-traces it also sanitizes the resulting
// OutputClaimData sequence into the shared fixture, so the telegram half can
// replay the recorded claims through its production transport. The drain runs
// in assert mode too: ack-triggered readiness events belong to the trace.
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

// normalizeClaimContent keeps golden claims deterministic: wall time is the
// only fragment inside card content that varies between runs.
func normalizeClaimContent(content string) string {
	return elapsedPattern.ReplaceAllString(content, "⌚ <elapsed>")
}

// sanitizeClaimAttributes keeps only deterministic host metadata; progress
// revisions hash wall-clock observations and the work directory embeds a
// temporary path — neither carries conversation meaning.
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

type scheduleBoundaryCommand uint8

const (
	deliverSubagentTick scheduleBoundaryCommand = iota
	deliverSubagentFresh
	deliverStoppedRoot
	stopRootAgain
	deliverDuplicateRoot
	deliverPendingResult
)

type scheduleBoundaryModel struct {
	rootStatus       sessionstore.SessionStatus
	rootRuns         int
	rootClaimed      bool
	rootPending      int
	subagentStatus   sessionstore.SessionStatus
	subagentMessages int
}

type scheduleBoundaryObservation struct {
	applied          bool
	errored          bool
	rootStatus       sessionstore.SessionStatus
	rootRuns         int
	rootPending      int
	subagentStatus   sessionstore.SessionStatus
	subagentMessages int
}

func (m *scheduleBoundaryModel) step(command scheduleBoundaryCommand) (bool, bool) {
	switch command {
	case deliverSubagentTick, deliverSubagentFresh:
		return false, false
	case deliverStoppedRoot:
		if m.rootClaimed {
			return false, false
		}

		m.rootClaimed = true
		m.rootStatus = sessionstore.SessionStatusCompleted
		m.rootRuns++
		return true, false
	case stopRootAgain:
		m.rootStatus = sessionstore.SessionStatusStopped
		return false, false
	case deliverDuplicateRoot:
		return false, false
	case deliverPendingResult:
		m.rootPending++
		return true, false
	default:
		panic("unknown schedule boundary command")
	}
}

func applyScheduleBoundaryCommand(
	t *testing.T,
	h *subagentHarness,
	rootID, subagentID int64,
	command scheduleBoundaryCommand,
) scheduleBoundaryObservation {
	t.Helper()

	applied, err := executeScheduleBoundaryCommand(t, h, rootID, subagentID, command)
	wantPending := 0

	if command == deliverPendingResult {
		wantPending = 1
	}

	require.Eventually(t, func() bool {
		pending, pendingErr := h.sessStore.ListPending(t.Context(), rootID)

		return pendingErr == nil && len(pending) == wantPending &&
			!h.mgr.HasActiveLoop(rootID) && !h.mgr.HasActiveLoop(subagentID)
	}, time.Second, 10*time.Millisecond)
	root, loadRootErr := h.sessStore.GetSession(t.Context(), rootID)
	require.NoError(t, loadRootErr)
	subagent, loadSubagentErr := h.sessStore.GetSession(t.Context(), subagentID)
	require.NoError(t, loadSubagentErr)
	pending, pendingErr := h.sessStore.ListPending(t.Context(), rootID)
	require.NoError(t, pendingErr)

	return scheduleBoundaryObservation{
		applied:          applied,
		errored:          err != nil,
		rootStatus:       root.Status,
		rootRuns:         countToolResultsFor(h.parentMessages(rootID), tool.IDSchedule),
		rootPending:      len(pending),
		subagentStatus:   subagent.Status,
		subagentMessages: len(h.parentMessages(subagentID)),
	}
}

func executeScheduleBoundaryCommand(
	t *testing.T,
	h *subagentHarness,
	rootID, subagentID int64,
	command scheduleBoundaryCommand,
) (bool, error) {
	t.Helper()

	switch command {
	case deliverSubagentTick:
		return enqueueScheduledInput(

			t.Context(), h.mgr.store,

			subagentID,
			"schedule:model:subagent-tick",
			"legacy task",
			false,
		)
	case deliverSubagentFresh:
		return enqueueScheduledInput(

			t.Context(), h.mgr.store,

			subagentID,
			"schedule:model:subagent-fresh",
			"legacy fresh task",
			true,
		)
	case deliverStoppedRoot, deliverDuplicateRoot:
		return enqueueScheduledInput(t.Context(), h.mgr.store, rootID, "schedule:model:root", "scheduled task", false)
	case stopRootAgain:
		return false, h.mgr.sendToSession(t.Context(), rootID, "/stop")
	case deliverPendingResult:
		applied, err := enqueueCallResult(

			t.Context(), h.mgr.store,

			rootID,
			"missing-call",
			tool.IDSleep,
			"must stay stopped",
		)

		return applied, err
	default:
		t.Fatalf("unknown command %d", command)

		return false, nil
	}
}

const batcherAgentFile = `---
name: batcher
description: Project subagent granted parallel reads only
tools:
  - read
  - batch
---
You are the batcher.
`

// childHoldRounds is where the scenario pauses the child mid-run: long enough
// for the spawn to settle, short enough to keep the recorded trace readable.
const childHoldRounds = 3

const childTotalRounds = 12

const promptReviewerAgentFile = `---
name: reviewer
description: Reviews changes before they ship
---
You are the project reviewer.
`

// promptRecorder keeps the system prompts each role's session was handed, in order.
type promptRecorder struct {
	mu     sync.Mutex
	byRole map[string][]string
}

func newPromptRecorder() *promptRecorder {
	return &promptRecorder{byRole: make(map[string][]string)}
}

func (r *promptRecorder) record(role, system string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.byRole[role] = append(r.byRole[role], system)
}

func (r *promptRecorder) first(t *testing.T, role string) string {
	t.Helper()

	r.mu.Lock()
	defer r.mu.Unlock()

	require.NotEmpty(t, r.byRole[role], "no %s request was recorded", role)

	return r.byRole[role][0]
}

const (
	registryChildMarker = "CHILD_REGISTRY"
	registryUseMarker   = "USE_REGISTRY_MCP"
)

func registryPromptRespond(fake *fakeMCPServer) func(string, []llmwire.Message) *llmwire.Response {
	return func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasUserContaining(messages, registryChildMarker) {
			return &llmwire.Response{Text: "child complete"}
		}

		if hasUserContaining(messages, registryUseMarker) {
			if hasToolResultForCallID(messages, "ping-next-activation") {
				return &llmwire.Response{Text: "mcp complete"}
			}

			return mcpPingCall("ping-next-activation")
		}

		if hasToolResultFor(messages, tool.IDMCPAdd) {
			if hasToolResultForCallID(messages, "ping-same-activation") {
				return &llmwire.Response{Text: "registered"}
			}

			return mcpPingCall("ping-same-activation")
		}

		if hasToolResultFor(messages, tool.IDTask) {
			return mcpToolCall("add-registry", tool.IDMCPAdd, fake.addParams("fake", "project"))
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{
			spawnTaskCall("task-registry", "explore", registryChildMarker),
		}}
	}
}

func assertInitialRegistryProjection(
	t *testing.T,
	h *subagentHarness,
	schemas *activationSchemas,
	prompts *promptRecorder,
	parentID, childID int64,
) {
	t.Helper()
	firstSchemas := schemas.first(t, parentID)
	for _, id := range append(dynamicRootTools(), configToolsForPromptScenario()...) {
		assert.Contains(t, firstSchemas, id, "root activation must expose daemon-registered %q", id)
	}
	assert.NotContains(t, firstSchemas, "mcp__fake__ping")

	firstPrompt := prompts.first(t, strconv.FormatInt(parentID, 10))
	assert.Contains(t, firstPrompt, "Sub-agents: task")
	assert.Contains(t, firstPrompt, "Scheduling: schedule")
	assert.Contains(t, firstPrompt, "# SCHEDULING")
	assert.NotContains(t, firstPrompt, "mcp__fake__ping")

	childSchemas := schemas.first(t, childID)
	for _, id := range []string{tool.IDTask, tool.IDSchedule, tool.IDSleep, tool.IDMCPAdd, tool.IDConfigEdit} {
		assert.NotContains(t, childSchemas, id, "child registry must gate %q", id)
	}
	childPrompt := prompts.first(t, strconv.FormatInt(childID, 10))
	assert.NotContains(t, childPrompt, "Sub-agents: task")
	assert.NotContains(t, childPrompt, "# SCHEDULING")
}

func assertNextRegistryProjection(
	t *testing.T,
	h *subagentHarness,
	schemas *activationSchemas,
	prompts *promptRecorder,
	parentID int64,
) {
	t.Helper()
	lastSchemas := schemas.last(t, parentID)
	assert.Contains(t, lastSchemas, "mcp__fake__ping")
	assert.Contains(t, lastSchemas, tool.IDTask)
	assert.Contains(t, lastSchemas, tool.IDConfigEdit)
	assert.Contains(t, toolResultForCallID(h.parentMessages(parentID), "ping-next-activation"), "pong from registry")

	lastPrompt := prompts.last(t, strconv.FormatInt(parentID, 10))
	assert.Contains(t, lastPrompt, "Sub-agents: task")
	assert.Contains(t, lastPrompt, "Scheduling: schedule")
}

func dynamicRootTools() []string {
	return []string{
		tool.IDTask, tool.IDSendToSubagent, tool.IDSleep, tool.IDSchedule,
		tool.IDMCPAdd, tool.IDMCPRemove, tool.IDMCPEnable, tool.IDMCPDisable, tool.IDMCPList,
	}
}

func configToolsForPromptScenario() []string {
	return []string{tool.IDConfigEdit}
}

// activationSchemas stores each request's inventory separately. A union would
// hide a stale registry that survived into a later activation.
type activationSchemas struct {
	mu   sync.Mutex
	byID map[int64][][]string
}

func (r *activationSchemas) record(sessionID int64, schemas []llmwire.ToolSchema) {
	r.mu.Lock()
	defer r.mu.Unlock()

	ids := make([]string, 0, len(schemas))
	for _, schema := range schemas {
		ids = append(ids, schema.Name)
	}
	r.byID[sessionID] = append(r.byID[sessionID], ids)
}

func (r *activationSchemas) first(t *testing.T, sessionID int64) []string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	require.NotEmpty(t, r.byID[sessionID], "no LLM request for session %d", sessionID)

	return append([]string(nil), r.byID[sessionID][0]...)
}

func (r *activationSchemas) last(t *testing.T, sessionID int64) []string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	require.NotEmpty(t, r.byID[sessionID], "no LLM request for session %d", sessionID)

	requests := r.byID[sessionID]
	return append([]string(nil), requests[len(requests)-1]...)
}

type registryPromptDeps struct {
	ctx          context.Context
	db           *sql.DB
	store        *sessionstore.Store
	sessionStore *sessionstore.Store
	links        subagent.Store
	schedules    schedule.Store
	mcpRegistry  mcpstore.Store
}

func (r *promptRecorder) last(t *testing.T, role string) string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	require.NotEmpty(t, r.byRole[role], "no %s request was recorded", role)

	requests := r.byRole[role]
	return requests[len(requests)-1]
}

func newRegistryPromptDeps(t *testing.T) registryPromptDeps {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "registry-prompt.db")
	db, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(ctx, db, dbPath))

	sessions := sessionstore.NewStore(db)
	return registryPromptDeps{
		ctx:          ctx,
		db:           db,
		store:        sessions,
		sessionStore: sessions,
		links: subagent.NewStore(
			db,
			sessions,
		),
		schedules:   schedule.NewStore(db, sessions),
		mcpRegistry: mcpstore.NewStore(db),
	}
}

// newRegistryPromptHarness is the composition-root MCP wiring over an ordinary
// project with a scripted LLM that records each live registry projection.
func newRegistryPromptHarness(
	t *testing.T,
	respond func(string, []llmwire.Message) *llmwire.Response,
) (*subagentHarness, *activationSchemas, *promptRecorder, mcpstore.Store) {
	t.Helper()

	deps := newRegistryPromptDeps(t)

	workDir := t.TempDir()
	cfg := &config.Config{WorkDir: workDir, Model: "fake-model"}
	recorder := &activationSchemas{byID: make(map[int64][][]string)}
	prompts := newPromptRecorder()

	factory := newRegistryPromptFactory(t, cfg, deps, respond, recorder, prompts)
	mgr := newRegistryPromptManager(deps, factory, t)
	projectID, err := deps.store.GetOrCreateProject(deps.ctx, workDir)
	require.NoError(t, err)

	return &subagentHarness{
		t: t, mgr: mgr, sessStore: deps.sessionStore, links: deps.links, schedStore: deps.schedules,
		projectID: projectID, ctx: deps.ctx,
	}, recorder, prompts, deps.mcpRegistry
}

func newRegistryPromptFactory(
	t *testing.T,
	cfg *config.Config,
	deps registryPromptDeps,
	respond func(string, []llmwire.Message) *llmwire.Response,
	recorder *activationSchemas,
	prompts *promptRecorder,
) sessionbuild.BuildInput {
	return scriptedBuildInput(t, cfg, deps.sessionStore, deps.mcpRegistry, func(_ *config.Config) (llm.Client, error) {
		return &registryPromptLLM{
			respond: respond, recorder: recorder, prompts: prompts,
		}, nil
	})
}

func newRegistryPromptManager(
	deps registryPromptDeps,
	factory sessionbuild.BuildInput,
	t *testing.T,
) *svc {
	mgr, _ := newScenarioDaemon(
		context.Background(),
		factory,
		deps.sessionStore,
		deps.links,
		budget.New(deps.sessionStore),
		schedule.NewService(deps.schedules, deps.sessionStore),
		func() string { return "fake-model" },
		deps.db,
	)
	mgr.mcpStore = deps.mcpRegistry
	mgr.applier = configapply.New(newTestConfigOps(t, t.TempDir()), deps.sessionStore)

	return mgr
}

func firstLine(s string) string {
	head, _, _ := strings.Cut(s, "\n")

	return head
}

type scheduleRestartHarness struct {
	*subagentHarness
	db        *sql.DB
	closeOnce sync.Once
	closeErr  error
}

type scheduleRunningObserver struct {
	source       sessionbus.Source
	subscription <-chan controllerapi.SessionNotification
	running      chan struct{}
	done         chan struct{}
	stopped      chan struct{}
	closeOnce    sync.Once
}

type orderedScheduleSender struct {
	schedule.SessionSender
	running <-chan struct{}
	done    <-chan struct{}
}

func dropTransientEvents(events []controllerapi.SessionNotification) []controllerapi.SessionNotification {
	kept := make([]controllerapi.SessionNotification, 0, len(events))

	for _, event := range events {
		if event.Notification.Type == sessionevent.NotifyStateChanged ||
			event.Notification.Type == sessionevent.NotifySessionCreated ||
			event.Notification.Message == "⚠️ Could not verify background work before releasing the active budget; the budget remains armed." {
			continue
		}

		kept = append(kept, event)
	}

	return kept
}

func deliverOneShotBeforeRestart(
	t *testing.T,
	h *scheduleRestartHarness,
	release chan<- struct{},
) (int64, *eventCollector) {
	t.Helper()
	events := collectEvents(h.mgr.bus.SubscribeManager("telegram-main"))
	t.Cleanup(events.stop)
	parentID := createScheduleSession(t, h, events)
	flaky := addFlakyDueOneShot(t, h, parentID)
	observer := newScheduleRunningObserver(t, h.mgr.bus)
	sender := &orderedScheduleSender{SessionSender: h.mgr, running: observer.running, done: observer.done}
	executor := schedule.NewExecutor(flaky, sender)
	executor.Start(h.ctx)
	t.Cleanup(executor.Stop)

	waitForScheduledInput(t, events, parentID)
	requireSignal(t, flaky.attempted)
	close(release)
	waitForVisibleMessage(t, events, parentID, "scheduled work completed")
	executor.Stop()

	requireOneShotRemainsRetryable(t, h, parentID)
	return parentID, events
}

func createScheduleSession(t *testing.T, h *scheduleRestartHarness, events *eventCollector) int64 {
	t.Helper()
	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "initialize", "fake-model", map[string]any{
		controllerapi.SessionAttributeManagerID: "telegram-main",
	})
	require.NoError(t, err)
	waitForVisibleMessage(t, events, parentID, "ready for schedule")

	return parentID
}

func addFlakyDueOneShot(t *testing.T, h *scheduleRestartHarness, sessionID int64) *failFirstRemoveScheduleStore {
	t.Helper()
	due := time.Now().Add(-time.Minute).UTC()
	_, err := h.schedStore.AddSchedule(h.ctx, sessionID, "", &due, "scheduled once", false)
	require.NoError(t, err)

	return &failFirstRemoveScheduleStore{Store: h.schedStore, attempted: make(chan struct{})}
}

func waitForScheduledInput(t *testing.T, events *eventCollector, sessionID int64) {
	t.Helper()
	events.waitFor(t, "scheduler input publication", func(got []controllerapi.SessionNotification) bool {
		for _, event := range got {
			if event.SessionID == sessionID && event.Notification.Type == sessionevent.NotifyInputReceived &&
				event.Notification.Source == "scheduler" && event.Notification.Message == "scheduled once" {
				return true
			}
		}

		return false
	})
}

func requireOneShotRemainsRetryable(t *testing.T, h *scheduleRestartHarness, sessionID int64) {
	t.Helper()
	remaining, err := h.schedStore.ListSchedules(h.ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, remaining, 1, "failed acknowledgement must leave the accepted one-shot retryable")
	assert.Equal(t, 1, countToolResultsFor(h.parentMessages(sessionID), tool.IDSchedule))
}

func retryOneShotAfterRestart(
	t *testing.T,
	h *scheduleRestartHarness,
	sessionID int64,
) *eventCollector {
	t.Helper()
	// Subscribe before Start: recovery announces the resumed runner, and a
	// subscription that loses that race drops the session_created trace event.
	events := collectEvents(h.mgr.bus.SubscribeManager("telegram-main"))
	t.Cleanup(events.stop)
	require.NoError(t, h.mgr.Start(h.ctx))
	executor := schedule.NewExecutor(h.schedStore, h.mgr)
	executor.Start(h.ctx)
	t.Cleanup(executor.Stop)

	require.Eventually(t, func() bool {
		schedules, err := h.schedStore.ListSchedules(h.ctx, sessionID)
		return err == nil && len(schedules) == 0 && !h.mgr.HasActiveLoop(sessionID)
	}, 5*time.Second, 10*time.Millisecond, "restart retry must acknowledge the accepted one-shot")
	executor.Stop()

	return events
}

func assertNoRetryPublication(t *testing.T, events []controllerapi.SessionNotification) {
	t.Helper()
	for _, event := range events {
		assert.NotEqual(t, sessionevent.NotifyInputReceived, event.Notification.Type,
			"duplicate scheduled delivery must not republish accepted input after restart")
		assert.NotEqual(t, sessionevent.NotifyMessage, event.Notification.Type,
			"duplicate scheduled delivery must not render another answer after restart")
	}
}

func newScheduleRunningObserver(t *testing.T, source sessionbus.Source) *scheduleRunningObserver {
	t.Helper()
	o := &scheduleRunningObserver{
		source: source, subscription: source.SubscribeManager("telegram-main"),
		running: make(chan struct{}), done: make(chan struct{}), stopped: make(chan struct{}),
	}
	go o.watch()
	t.Cleanup(o.close)

	return o
}

func (o *scheduleRunningObserver) watch() {
	defer close(o.stopped)
	for {
		select {
		case <-o.done:
			return
		case event := <-o.subscription:
			if event.Notification.Type == sessionevent.NotifyStateChanged &&
				event.Notification.Status == controllerapi.StateRunning {
				o.closeOnce.Do(func() { close(o.running) })
			}
		}
	}
}

func (o *scheduleRunningObserver) close() {
	o.closeOnce.Do(func() { close(o.running) })
	close(o.done)
	o.source.UnsubscribeManager(o.subscription)
	<-o.stopped
}

func (s *orderedScheduleSender) NotifySession(sessionID int64, n sessionevent.Notification) {
	if n.Type == sessionevent.NotifyInputReceived && n.Source == "scheduler" {
		select {
		case <-s.running:
		case <-s.done:
			return
		}
	}

	s.SessionSender.NotifySession(sessionID, n)
}

func newScheduleRestartHarness(
	t *testing.T,
	dbPath, workDir string,
	respond func(string, []llmwire.Message) *llmwire.Response,
) *scheduleRestartHarness {
	t.Helper()
	db := openScheduleRestartDB(t, dbPath)
	h := &scheduleRestartHarness{db: db}
	h.subagentHarness = buildScheduleRestartHarness(t, db, workDir, respond)
	t.Cleanup(func() { require.NoError(t, h.close()) })

	return h
}

func openScheduleRestartDB(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	db, err := migrate.OpenDB(context.Background(), dbPath)
	require.NoError(t, err)
	require.NoError(t, migrate.Run(context.Background(), db, dbPath))

	return db
}

func buildScheduleRestartHarness(
	t *testing.T,
	db *sql.DB,
	workDir string,
	respond func(string, []llmwire.Message) *llmwire.Response,
) *subagentHarness {
	t.Helper()
	store := sessionstore.NewStore(db)
	sessionStore := sessionstore.NewStore(db)
	links := subagent.NewStore(db, sessionStore)
	schedules := schedule.NewStore(db, sessionStore)
	factory := scheduleRestartFactory(t, workDir, sessionStore, respond)
	mgr, _ := newScenarioDaemon(
		context.Background(),
		factory,
		sessionStore,
		links,
		budget.New(sessionStore),
		schedule.NewService(schedules, sessionStore),
		func() string {
			return "fake-model"
		},
		db,
	)
	projectID, err := store.GetOrCreateProject(context.Background(), workDir)
	require.NoError(t, err)

	return &subagentHarness{
		t: t, mgr: mgr, sessStore: sessionStore, links: links, schedStore: schedules,
		projectID: projectID, ctx: context.Background(),
	}
}

func scheduleRestartFactory(
	t *testing.T,
	workDir string,
	store *sessionstore.Store,
	respond func(string, []llmwire.Message) *llmwire.Response,
) sessionbuild.BuildInput {
	cfg := &config.Config{WorkDir: workDir, Model: "fake-model"}
	return scriptedBuildInput(t, cfg, store, nil, func(*config.Config) (llm.Client, error) {
		return &scriptedLLM{respond: respond}, nil
	})
}

func (h *scheduleRestartHarness) close() error {
	h.closeOnce.Do(func() {
		h.shutdown()
		h.closeErr = h.db.Close()
	})

	return h.closeErr
}

func waitForWaitKind(
	t *testing.T,
	collector *eventCollector,
	sessionID int64,
	kind sessionevent.WaitKind,
) {
	t.Helper()
	collector.waitFor(
		t,
		fmt.Sprintf("waiting kind %s", kind),
		func(events []controllerapi.SessionNotification) bool {
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
		},
	)
}

func waitForSubagentSet(
	t *testing.T,
	collector *eventCollector,
	sessionID int64,
	want []int64,
) {
	t.Helper()
	collector.waitFor(
		t,
		fmt.Sprintf("subagent wait set %v", want),
		func(events []controllerapi.SessionNotification) bool {
			for _, event := range events {
				if event.SessionID != sessionID || event.Notification.Type != sessionevent.NotifyWaiting {
					continue
				}

				var got []int64
				for _, item := range event.Notification.Waiting {
					if item.Kind != sessionevent.WaitSubagent {
						return false
					}
					got = append(got, item.ChildID)
				}
				slices.Sort(got)
				if slices.Equal(got, want) {
					return true
				}
			}

			return false
		},
	)
}

func waitForVisibleMessage(
	t *testing.T,
	collector *eventCollector,
	sessionID int64,
	message string,
) {
	waitForVisibleMessageCount(t, collector, sessionID, message, 1)
}

func waitForVisibleMessageCount(
	t *testing.T,
	collector *eventCollector,
	sessionID int64,
	message string,
	want int,
) {
	t.Helper()
	collector.waitFor(t, message, func(events []controllerapi.SessionNotification) bool {
		return countPublishedMessage(events, sessionID, message) == want
	})
}

func waitForIdleAfterMessage(
	t *testing.T,
	collector *eventCollector,
	sessionID int64,
	message string,
) {
	t.Helper()
	collector.waitFor(t, "idle after "+message, func(events []controllerapi.SessionNotification) bool {
		messageSeen := false
		for _, event := range events {
			if event.SessionID != sessionID {
				continue
			}

			if event.Notification.Type == sessionevent.NotifyMessage && event.Notification.Message == message {
				messageSeen = true
			}

			if messageSeen && event.Notification.Type == sessionevent.NotifyStateChanged &&
				event.Notification.Status == sessionevent.StateIdle {
				return true
			}
		}

		return false
	})
}

const untrustedProbeMarker = "PROBE_UNTRUSTED"

// requestRecorder keeps each request's full message transcript, in order.
type requestRecorder struct {
	mu   sync.Mutex
	msgs [][]llmwire.Message
}

func (r *requestRecorder) record(msgs []llmwire.Message) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.msgs = append(r.msgs, msgs)
}

func (r *requestRecorder) lastMessages(t *testing.T) []llmwire.Message {
	t.Helper()

	r.mu.Lock()
	defer r.mu.Unlock()

	require.NotEmpty(t, r.msgs, "no scripted model request was recorded")

	return r.msgs[len(r.msgs)-1]
}

// waitTimeLayout is deliberately a copy of the layout sessionevent renders wake
// times with: a change there must break the golden, not be normalized away.
const (
	waitTimeLayout     = "15:04 02 Jan"
	wakePlaceholder    = "<wake>"
	workDirPlaceholder = "<workdir>"
	// The session name embeds the temp project directory, which carries no
	// scenario meaning and changes with unrelated harness edits.
	namePlaceholder = "<name>"
)

// updateHarnessTraces rewrites the recorded goldens instead of asserting them:
// go test ./internal/daemon -run TestHarnessScenario -update-traces
var updateHarnessTraces = flag.Bool(
	"update-traces", false, "rewrite the recorded controller traces under internal/testdata",
)

var childRefPattern = regexp.MustCompile(`#(\d+)`)

// elapsedPattern matches the wall-time fragment of an automatic card; its
// value depends on scheduling and carries no conversation meaning.
var elapsedPattern = regexp.MustCompile(`⌚ [0-9.]+[a-z0-9.]+`)

// harnessTraceFile is the shared artifact: the exact ordered notification trace
// a daemon scenario published to a controller sink, plus the exact sanitized
// controllerapi.OutputClaimData sequence the production controller produced for
// the same run. The telegram manager scenario replays these claims through the
// production output transport. Ids are normalized; generations are kept.
type harnessTraceFile struct {
	SourceTest string              `json:"source_test"`
	Trace      []harnessTraceEvent `json:"trace"`
	Claims     []harnessTraceClaim `json:"claims,omitempty"`
}

// messagePlaceholderPrefix marks sanitized Telegram message receipts. The
// telegram replay maps each placeholder to a real message id, so edit decisions
// and chunk bookkeeping run against genuine targets.
const messagePlaceholderPrefix = "<msg-"

// harnessTraceClaim is the sanitized OutputClaimData one production claim
// produced. Presence-bearing generations stay exact; random attempt ids and raw
// message receipts do not survive sanitization.
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

// harnessTracePath resolves the shared fixture root. Both this package and
// internal/managers/telegram read the same files, so neither half can drift.
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

// readHarnessTraceFileForUpdate loads a trace file during recording, tolerating
// a file that only the notification pass has written so far.
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
			// Heartbeats are 1-second periodic noise: their count depends on
			// wall-clock speed, not on the conversation (the renderer only
			// derives ephemeral typing from them). Recorded goldens stay
			// deterministic by dropping them here.
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

// requireRecordableNotification fails loudly instead of silently dropping a
// payload the shared trace schema cannot carry to the manager half.
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

const helpWithGWT = "## Session commands\n" +
	"`/status` — show session status\n" +
	"`/stop` — stop the current run\n" +
	"`/clear` — start a fresh session\n" +
	"`/kill` — close this session\n" +
	"`/compact [focus]` — compact the context\n" +
	"`/schedules` — list schedules\n" +
	"`/budget <request>` — arm, replace, inspect, or clear a one-shot cost/wall-time checkpoint\n" +
	"`/gwt <name>` — fork into a worktree (Telegram session topics only)"

func runAcceptedInputRestartScenario(
	t *testing.T,
	afterInput func(*testing.T, *subagentHarness, int64),
	traceName, sourceTest string,
) {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "accepted-input.db")
	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasUserContaining(messages, "accepted before crash") {
			return &llmwire.Response{Text: "accepted input recovered"}
		}

		return &llmwire.Response{Text: "unexpected prompt"}
	}

	first := newSubagentHarnessOnDB(t, dbPath, respond, nil)
	root, err := first.sessStore.CreateSession(first.ctx, first.projectID, "fake-model", "", nil)
	require.NoError(t, err)
	input, err := first.sessStore.Enqueue(
		first.ctx,
		sessionstore.Input{SessionID: root.ID, Source: sessionstore.InputSourceUser, Content: "accepted before crash"},
	)
	require.NoError(t, err)
	_, err = first.sessStore.Commit(
		first.ctx,
		sessionstore.Commit{
			SessionID: input.Input.SessionID,
			Accept: []sessionstore.Accept{
				{
					InputID:    input.Input.ID,
					State:      sessionstore.InputStateAccepted,
					Content:    "[user] accepted before crash",
					LinkRef:    -1,
					ModelBound: true,
				},
			},
		},
	)
	require.NoError(t, err)
	if afterInput != nil {
		afterInput(t, first, root.ID)
	}
	first.shutdown()

	second := newSubagentHarnessOnDB(t, dbPath, respond, nil)
	collector := collectEvents(second.mgr.bus.SubscribeAll())
	defer func() {
		collector.stop()
		second.shutdown()
	}()

	second.startInboxWake()
	second.mgr.resumeAfterRestart(second.ctx)
	waitForVisibleMessage(t, collector, root.ID, "accepted input recovered")
	waitForIdleAfterMessage(t, collector, root.ID, "accepted input recovered")

	assert.Equal(t, "accepted input recovered", lastAssistantTextDTO(second.parentMessages(root.ID)))
	require.NoError(t, llm.ValidateToolPairing(second.parentMessages(root.ID)))
	assertHarnessTraceForScenario(t, sourceTest, traceName, collector.snapshot(), root.ID)
}

func appendCrashToolProgress(t *testing.T, h *subagentHarness, sessionID int64) {
	t.Helper()

	calls, err := json.Marshal([]llmwire.ToolCall{{
		ID: "crash-tool", Name: "read", Arguments: []byte(`{"path":"README.md"}`),
	}})
	require.NoError(t, err)
	_, err = h.sessStore.Commit(h.ctx, sessionstore.Commit{SessionID: sessionID, Messages: []*transcript.Message{{
		Role: llmwire.RoleAssistant, ToolCalls: calls,
	}}})
	require.NoError(t, err)
	_, err = h.sessStore.Commit(h.ctx, sessionstore.Commit{SessionID: sessionID, Messages: []*transcript.Message{{
		Role: llmwire.RoleTool, ToolCallID: "crash-tool", ToolName: "read", Content: "durable tool result",
	}}})
	require.NoError(t, err)
}

// contextInfo snapshots the seam observation the raw client can make: what the
// runner actually handed the session loop.
type contextInfo struct {
	hasDeadline bool
}

// subagentHarness wires a real sessionbuild.BuildInput (fake LLM) + daemon svc over a
// temp SQLite DB.
type subagentHarness struct {
	t          *testing.T
	db         *sql.DB
	mgr        *svc
	sessStore  *sessionstore.Store
	links      subagent.Store
	schedStore schedule.Store
	projectID  int64
	ctx        context.Context

	llmMu    sync.Mutex
	llmRefs  []*scriptedLLM // every client the session factory created
	wakeOnce sync.Once
}

func (h *subagentHarness) startInboxWake() {
	h.wakeOnce.Do(func() { h.mgr.startWake() })
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

	if hasToolResultFor(msgs, "task") || hasUserContaining(msgs, "<subagent_completion>") {
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
	return newSubagentHarnessOnDBWithProjectConfig(
		t, dbPath, respond, decorate, nil,
	)
}

func newSubagentHarnessOnDBWithProjectConfig(
	t *testing.T,
	dbPath string,
	respond func(system string, msgs []llmwire.Message) *llmwire.Response,
	decorate func(subagent.Store) subagent.Store,
	configure func(*config.Config),
) *subagentHarness {
	t.Helper()
	if _, err := coagenthome.UserHome(); err != nil {
		t.Setenv("HOME", t.TempDir())
	}

	db, err := migrate.OpenDB(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(context.Background(), db, dbPath))

	store := sessionstore.NewStore(db)
	sessStore := sessionstore.NewStore(db)
	links := subagent.NewStore(db, sessStore)
	schedStore := schedule.NewStore(db, sessStore)

	if decorate != nil {
		links = decorate(links)
	}

	workDir := t.TempDir()
	cfg := &config.Config{WorkDir: workDir, Model: "fake-model"}
	if configure != nil {
		configure(cfg)
	}

	var (
		pid int64
		h   = &subagentHarness{
			t: t, db: db, sessStore: sessStore, links: links, schedStore: schedStore,
			ctx: context.Background(),
		}
	)
	pid, err = store.GetOrCreateProject(context.Background(), workDir)
	require.NoError(t, err)

	factory := scriptedBuildInput(t, cfg, sessStore, nil, func(_ *config.Config) (llm.Client, error) {
		client := &scriptedLLM{respond: respond}

		h.llmMu.Lock()
		h.llmRefs = append(h.llmRefs, client)
		h.llmMu.Unlock()

		return client, nil
	})

	mgr, _ := newScenarioDaemon(
		context.Background(),
		factory,
		sessStore,
		links,
		budget.New(sessStore),
		schedule.NewService(schedStore, sessStore),
		func() string { return "fake-model" },
		db,
	)

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

func (h *subagentHarness) waitForParentCompletions(parentID, childID int64, want int) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if countSubagentCompletions(h.parentMessages(parentID), childID) >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %d parent inbox completions for child %d", want, childID)
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

func lastToolResultContent(msgs []llmwire.Message, toolName string) string {
	for _, v := range slices.Backward(msgs) {
		if v.Role == llmwire.RoleTool && v.ToolName == toolName {
			return v.Content
		}
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

func lastSubagentCompletion(messages []llmwire.Message, childID int64) string {
	needle := "child_id: " + strconv.FormatInt(childID, 10)
	for _, message := range slices.Backward(messages) {
		if message.Role == llmwire.RoleUser && strings.Contains(message.Content, "<subagent_completion>") &&
			strings.Contains(message.Content, needle) {
			return message.Content
		}
	}

	return ""
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

// newMCPHarness wires one registry store into session tool stacks.
func newMCPHarness(
	t *testing.T,
	respond func(system string, msgs []llmwire.Message) *llmwire.Response,
) (*subagentHarness, mcpstore.Store) {
	return newMCPHarnessConfigured(t, respond, nil)
}

func newMCPHarnessConfigured(
	t *testing.T,
	respond func(system string, msgs []llmwire.Message) *llmwire.Response,
	configure func(*config.Config),
) (*subagentHarness, mcpstore.Store) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")

	db, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(ctx, db, dbPath))

	store := sessionstore.NewStore(db)
	sessStore := sessionstore.NewStore(db)
	links := subagent.NewStore(db, sessStore)
	schedStore := schedule.NewStore(db, sessStore)
	registry := mcpstore.NewStore(db)

	workDir := t.TempDir()
	cfg := &config.Config{WorkDir: workDir, Model: "fake-model"}
	if configure != nil {
		configure(cfg)
	}

	factory := scriptedBuildInput(t, cfg, sessStore, registry, func(_ *config.Config) (llm.Client, error) {
		return &scriptedLLM{respond: respond}, nil
	})

	mgr, _ := newScenarioDaemon(
		context.Background(),
		factory,
		sessStore,
		links,
		budget.New(sessStore),
		schedule.NewService(schedStore, sessStore),
		func() string { return "fake-model" },
		db,
	)
	mgr.mcpStore = registry

	pid, err := store.GetOrCreateProject(ctx, workDir)
	require.NoError(t, err)

	return &subagentHarness{
		t: t, db: db, mgr: mgr, sessStore: sessStore, links: links, schedStore: schedStore,
		projectID: pid, ctx: ctx,
	}, registry
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

func seedChildLink(ctx context.Context, sessions *sessionstore.Store, link subagent.Link) error {
	if link.State == "" {
		link.State = subagent.StateSpawned
	}
	return sessions.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO subagent_links
		(parent_id,child_id,task_call_id,blocking,depth,state,created_at,result,outcome) VALUES (?,?,?,?,?,?,?,?,?)`,
			link.ParentID, link.ChildID, link.TaskCallID, link.Blocking, link.Depth, link.State,
			time.Now().UTC().Unix(), link.Result, link.Outcome)
		return err
	})
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

func finalizeTestChild(ctx context.Context, t *testing.T, manager *svc, childID int64) {
	t.Helper()
	unlock, err := manager.lockSessionTree(ctx, childID)
	require.NoError(t, err)
	deliver := manager.finalizeChildLocked(ctx, childID, false)
	unlock()
	if deliver != nil {
		deliver()
	}
}

func countSilenceIntents(t *testing.T, h *subagentHarness, sessionID int64) int {
	t.Helper()

	var count int
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM session_outbox
		WHERE session_id = ? AND source_key LIKE 'progress:silence:%'`, sessionID).Scan(&count))

	return count
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

func topicAttrNumber(raw any) int64 {
	switch v := raw.(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case float64:
		return int64(v)
	default:
		return 0
	}
}

type mockSession struct {
	*scriptedLLM
	mu            sync.Mutex
	ran           bool
	runErr        error
	completeAfter time.Duration
}

type mockFactory struct {
	mu            sync.Mutex
	sessions      []*mockSession
	nextSess      *mockSession
	createErrOnce error
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
	if f.createErrOnce != nil {
		err := f.createErrOnce
		f.createErrOnce = nil
		return nil, err
	}
	client := f.nextSess
	f.nextSess = nil
	if client == nil {
		client = &mockSession{}
	}
	client.scriptedLLM = &scriptedLLM{}
	f.sessions = append(f.sessions, client)
	return client, nil
}

func newTestManager(t *testing.T) (*svc, *mockFactory, *sessionstore.Store) {
	t.Helper()
	mgr, factory, store, _ := newTestManagerWithSchedule(t)
	return mgr, factory, store
}

func newTestManagerWithSchedule(t *testing.T) (*svc, *mockFactory, *sessionstore.Store, schedule.Store) {
	t.Helper()
	restore := coagenthome.Override(t.TempDir())
	t.Cleanup(restore)
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := migrate.OpenDB(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(t.Context(), db, dbPath))
	store := sessionstore.NewStore(db)
	schedules := schedule.NewStore(db, store)
	factory := &mockFactory{}
	cfg := &config.Config{
		Model:   "fake-model",
		WorkDir: t.TempDir(),
		UnifiedConfig: &config.UnifiedConfig{
			Models: []config.ModelEntry{
				{ID: "fake-model"},
				{ID: "test-model"},
				{ID: "my-model"},
				{ID: "old-model"},
				{ID: "new-model"},
			},
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
	in := scriptedBuildInput(t, cfg, store, nil, factory.client)
	mgr, _ := newScenarioDaemon(
		t.Context(),
		in,
		store,
		subagent.NewStore(db, store),
		budget.New(store),
		schedule.NewService(schedules, store),
		func() string { return "fake-model" },
		db,
	)
	t.Cleanup(func() { mgr.Shutdown(3 * time.Second) })
	return mgr, factory, store, schedules
}

// waitForLoopStart blocks until a "running" state_changed notification arrives.
func waitForLoopStart(
	t *testing.T,
	ch <-chan controllerapi.SessionNotification,
	sessionID int64,
	timeout time.Duration,
) {
	t.Helper()
	waitForState(t, ch, sessionID, controllerapi.StateRunning, timeout)
}

func waitForPendingInput(
	t *testing.T,
	store *sessionstore.Store,
	sessionID int64,
	want bool,
	timeout time.Duration,
) {
	t.Helper()
	require.Eventually(t, func() bool {
		_, err := store.PeekPending(context.Background(), sessionID)
		if want {
			return err == nil
		}

		return errors.Is(err, sessionstore.ErrNoPendingInput)
	}, timeout, 10*time.Millisecond)
}

// waitForState blocks until a specific state_changed notification arrives.
func waitForState(
	t *testing.T,
	ch <-chan controllerapi.SessionNotification,
	sessionID int64,
	want controllerapi.State,
	timeout time.Duration,
) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case sn := <-ch:
			if sn.SessionID != sessionID {
				continue
			}
			if sn.Notification.Type == sessionevent.NotifyStateChanged && sn.Notification.Status == want {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for state %q on session %d", want, sessionID)
		}
	}
}

// shiftingMCPScript logs process exit and changes tools/list when the fixture allows it.
const shiftingMCPScript = `#!/bin/sh
LOG="$1"
PONG="$2"
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
      echo call >> "$LOG"
      printf '{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"%s"}]}}\n' "$id" "$PONG"
      ;;
    *)
      printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"Method not found"}}\n' "$id"
      ;;
  esac
done
`

func lastUserText(msgs []llmwire.Message) string {
	const nudgePrefix = "You ended your previous response without calling a tool."

	text := ""

	for _, m := range msgs {
		// The host completion nudge is not a user turn: the confirmation turn
		// must answer the task prompt again, not fall through to the default
		// registration branch.
		if m.Role == llmwire.RoleUser && !strings.HasPrefix(m.Content, nudgePrefix) {
			text = m.Content
		}
	}

	return text
}

type registryModelCommand string

const (
	registryAdd     registryModelCommand = "add"
	registryRebuild registryModelCommand = "rebuild"
	registryDisable registryModelCommand = "disable"
	registryRelease registryModelCommand = "release"
	registryEnable  registryModelCommand = "enable"
	registryRemove  registryModelCommand = "remove"
	registryRestart registryModelCommand = "restart"
)

type registryReference struct {
	registered bool
	enabled    bool
	visible    bool
}

type registryModelHarness struct {
	db        *sql.DB
	store     mcpstore.Store
	projectID int64
	workDir   string
	fake      *exitTrackingMCPServer
	service   mcp.Service
	reference registryReference
}

func newRegistryModelHarness(t *testing.T) *registryModelHarness {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "mcp-registry-model.db")
	db, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	require.NoError(t, migrate.Run(ctx, db, dbPath))

	workDir := t.TempDir()
	projectStore := sessionstore.NewStore(db)
	projectID, err := projectStore.GetOrCreateProject(ctx, workDir)
	require.NoError(t, err)

	fake := newExitTrackingMCPServer(t, "pong from model")
	return &registryModelHarness{
		db:        db,
		store:     mcpstore.NewStore(db),
		projectID: projectID,
		workDir:   workDir,
		fake:      fake,
	}
}

func (h *registryModelHarness) apply(t *testing.T, command registryModelCommand) {
	t.Helper()
	ctx := context.Background()

	switch command {
	case registryAdd:
		err := h.store.Add(ctx, &h.projectID, mcpstore.ServerDef{
			Name: "fake", Command: h.fake.path, Args: h.fake.args(), Enabled: true,
		})
		require.NoError(t, err)
		h.reference.registered = true
		h.reference.enabled = true
	case registryRebuild:
		h.rebuild(t)
	case registryDisable:
		require.NoError(t, h.store.SetEnabled(ctx, &h.projectID, "fake", false))
		h.reference.enabled = false
	case registryRelease:
		if h.service != nil {
			h.service.Stop()
			h.service = nil
		}
		h.reference.visible = false
	case registryEnable:
		require.NoError(t, h.store.SetEnabled(ctx, &h.projectID, "fake", true))
		h.reference.enabled = true
	case registryRemove:
		require.NoError(t, h.store.Remove(ctx, &h.projectID, "fake"))
		h.reference.registered = false
		h.reference.enabled = false
	case registryRestart:
		if h.service != nil {
			h.service.Stop()
			h.service = nil
		}
		h.reference.visible = false
	default:
		t.Fatalf("unknown registry model command %q", command)
	}
}

func (h *registryModelHarness) rebuild(t *testing.T) {
	t.Helper()
	defs, err := h.store.ListForProject(context.Background(), h.projectID)
	require.NoError(t, err)
	configs := make(map[string]mcp.ServerConfig, len(defs))
	for _, def := range defs {
		configs[def.Name] = mcp.ServerConfig{Command: def.Command, Args: def.Args, Env: def.Env}
	}

	service, err := mcp.AcquireForWorkDir(context.Background(), configs, h.workDir, nil, nil)
	require.NoError(t, err)
	h.service = service
	actual := service != nil && service.Stats().Started == 1
	expected := h.reference.registered && h.reference.enabled
	assert.Equal(t, expected, actual, "real registry projection and stack acquire")
	h.reference.visible = expected
}

func (h *registryModelHarness) assertVisible(t *testing.T, want bool, message string) {
	t.Helper()
	assert.Equal(t, want, h.reference.visible, "reference model: "+message)
	actual := h.service != nil && h.service.Stats().Started == 1
	assert.Equal(t, want, actual, "real registry/stack state: "+message)
}

func (h *registryModelHarness) close() {
	if h.service != nil {
		h.service.Stop()
	}
	_ = h.db.Close()
}

// fakeMCPScript is a stdio MCP server: answers the handshake, advertises one tool
// and answers it. Every spawn and call is logged so stack ownership is observable.
const fakeMCPScript = `#!/bin/sh
LOG="$1"
PONG="$2"
RELEASE="$3"
echo spawn >> "$LOG"
trap 'echo exit >> "$LOG"' EXIT
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  [ -n "$id" ] || continue
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2024-11-05","capabilities":{"tools":{}},"serverInfo":{"name":"fakemcp","version":"0.0.1"}}}\n' "$id"
      ;;
    *'"method":"tools/list"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"ping","description":"Answers pong.","inputSchema":{"type":"object","properties":{}}}]}}\n' "$id"
      ;;
    *'"method":"tools/call"'*)
      echo call >> "$LOG"
      if [ -n "$RELEASE" ]; then
        while [ ! -f "$RELEASE" ]; do sleep 0.05; done
      fi
      printf '{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"%s"}]}}\n' "$id" "$PONG"
      ;;
    *)
      printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"Method not found"}}\n' "$id"
      ;;
  esac
done
`

func mcpPingCall(id string) *llmwire.Response {
	return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
		ID: id, Name: "mcp__fake__ping", Arguments: []byte(`{}`),
	}}}
}

func mcpToolCall(id, name, params string) *llmwire.Response {
	return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
		ID: id, Name: name, Arguments: []byte(params),
	}}}
}

func hasToolResultForCallID(msgs []llmwire.Message, callID string) bool {
	for _, m := range msgs {
		if m.Role == llmwire.RoleTool && m.ToolCallID == callID {
			return true
		}
	}

	return false
}

func toolResultForCallID(msgs []llmwire.Message, callID string) string {
	for _, m := range msgs {
		if m.Role == llmwire.RoleTool && m.ToolCallID == callID {
			return m.Content
		}
	}

	return ""
}

type mcpRestartHarness struct {
	*subagentHarness
	db       *sql.DB
	registry mcpstore.Store
}

func newMCPRestartHarness(
	t *testing.T,
	dbPath string,
	workDir string,
	respond func(string, []llmwire.Message) *llmwire.Response,
) *mcpRestartHarness {
	t.Helper()
	ctx := context.Background()
	db, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	require.NoError(t, migrate.Run(ctx, db, dbPath))

	store := sessionstore.NewStore(db)
	sessStore := sessionstore.NewStore(db)
	links := subagent.NewStore(db, sessStore)
	schedStore := schedule.NewStore(db, sessStore)
	registry := mcpstore.NewStore(db)
	cfg := &config.Config{WorkDir: workDir, Model: "fake-model"}
	factory := scriptedBuildInput(t, cfg, sessStore, registry, func(_ *config.Config) (llm.Client, error) {
		return &scriptedLLM{respond: respond}, nil
	})
	mgr, _ := newScenarioDaemon(
		context.Background(),
		factory,
		sessStore,
		links,
		budget.New(sessStore),
		schedule.NewService(schedStore, sessStore),
		func() string { return "fake-model" },
		db,
	)
	mgr.mcpStore = registry
	projectID, err := store.GetOrCreateProject(ctx, workDir)
	require.NoError(t, err)

	h := &mcpRestartHarness{
		subagentHarness: &subagentHarness{
			t: t, mgr: mgr, sessStore: sessStore, links: links, schedStore: schedStore,
			projectID: projectID, ctx: ctx,
		},
		db:       db,
		registry: registry,
	}
	t.Cleanup(h.close)

	return h
}

func (h *mcpRestartHarness) close() {
	h.mgr.Shutdown(5 * time.Second)
	_ = h.db.Close()
}

const orphanTaskCallID = "orphan-task-1"

// modelRequests records every transcript the provider was shown, so a test can
// assert no request ever carried a dangling tool_use.
type modelRequests struct {
	mu   sync.Mutex
	seen [][]llmwire.Message
}

// askForBlockingTaskRespond parks the session on a blocking child once, then reacts
// to whatever came back for it.
func askForBlockingTaskRespond(_ string, msgs []llmwire.Message) *llmwire.Response {
	if hasToolResultFor(msgs, tool.IDTask) {
		return &llmwire.Response{Text: "noted: " + lastToolResultContent(msgs, tool.IDTask)}
	}

	return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
		ID:        orphanTaskCallID,
		Name:      tool.IDTask,
		Arguments: []byte(`{"prompt":"do the thing","description":"orphan probe","subagent_type":"general"}`),
	}}}
}

// newExternalCallDaemon is one daemon image with a config applier wired, so a
// session can suspend on an external call and on config_edit.
func newExternalCallDaemon(
	t *testing.T,
	dbPath, configDir string,
	respond func(string, []llmwire.Message) *llmwire.Response,
) *applyDaemon {
	t.Helper()

	h := newSubagentHarnessOnDB(t, dbPath, respond, nil)
	ops := configops.New(filepath.Join(configDir, "config.yaml"), filepath.Join(configDir, "secrets"))

	h.mgr.applier = configapply.New(ops, h.sessStore)

	return &applyDaemon{subagentHarness: h, ops: ops, restarts: h.mgr.applier.Restart()}
}

// stageTaskAndStop parks a session on a blocking child, marks the child link
// killed (a child that did not survive the shutdown), and takes the daemon down
// with the task call dangling in the transcript.
func stageTaskAndStop(t *testing.T, dbPath, configDir string, seen *modelRequests) int64 {
	t.Helper()

	first := newExternalCallDaemon(t, dbPath, configDir, seen.wrap(askForBlockingTaskRespond))

	first.startInboxWake()
	sessionID, err := first.mgr.Send(
		first.ctx, first.projectID, "do work then spawn", "fake-model", nil,
	)
	require.NoError(t, err)

	first.waitUntil("the parent parked on the child", func() bool {
		return countAssistantToolCallsFor(first.parentMessages(sessionID), tool.IDTask) == 1 &&
			!first.mgr.HasActiveLoop(sessionID)
	})

	msgs := first.parentMessages(sessionID)
	require.Equal(t, 1, countAssistantToolCallsFor(msgs, tool.IDTask))
	require.Zero(t, countToolResultsFor(msgs, tool.IDTask), "the task is out with the world")

	link, err := first.links.GetLinkByTaskCallID(first.ctx, sessionID, orphanTaskCallID)
	require.NoError(t, err)
	require.NotNil(t, link, "the suspended parent owes its call to a child link")

	// The producer ledger is the link row; a child that did not survive the
	// restart owns nothing, so its link must not claim the call either.
	_, err = first.db.ExecContext(
		first.ctx, `DELETE FROM subagent_links WHERE child_id = ?`, link.ChildID,
	)
	require.NoError(t, err)

	first.shutdown()

	return sessionID
}

// unresolvedExternalCallsByName is the reference view of what is pending: the
// external calls a provider can see dangling in the transcript, derived from the
// transcript alone.
func unresolvedExternalCallsByName(msgs []llmwire.Message) map[string]string {
	resolved := make(map[string]bool)

	for _, m := range msgs {
		if m.Role == llmwire.RoleTool && m.ToolCallID != "" {
			resolved[m.ToolCallID] = true
		}
	}

	out := make(map[string]string)

	for _, m := range msgs {
		if m.Role != llmwire.RoleAssistant {
			continue
		}

		for _, tc := range m.ToolCalls {
			if tc.ID != "" && !resolved[tc.ID] && tool.IsExternalCall(tc.Name) {
				out[tc.ID] = tc.Name
			}
		}
	}

	return out
}

func (r *modelRequests) wrap(
	respond func(string, []llmwire.Message) *llmwire.Response,
) func(string, []llmwire.Message) *llmwire.Response {
	return func(system string, msgs []llmwire.Message) *llmwire.Response {
		r.mu.Lock()
		r.seen = append(r.seen, slices.Clone(msgs))
		r.mu.Unlock()

		return respond(system, msgs)
	}
}

func (r *modelRequests) assertAllPaired(t *testing.T) {
	t.Helper()

	r.mu.Lock()
	defer r.mu.Unlock()

	for i, req := range r.seen {
		assert.NoErrorf(t, llm.ValidateToolPairing(req), "request %d reached the provider with a dangling tool_use", i)
	}
}

// parkedOnChild reports the durable park, not the in-memory loop flag: between
// two model iterations the loop briefly counts as inactive, and a wait keyed on
// that flag races the next iteration on a fast machine.
func (d *applyDaemon) parkedOnChild(sessionID int64) bool {
	rec, err := d.sessStore.GetSession(d.ctx, sessionID)
	if err != nil || rec.Status != sessionstore.SessionStatusSuspended {
		return false
	}

	return countAssistantToolCallsFor(d.parentMessages(sessionID), tool.IDTask) == 1
}

func storedAssistant(toolCalls string) *transcript.Message {
	return &transcript.Message{Role: "assistant", ToolCalls: []byte(toolCalls)}
}

// scenarioManagerID is the manager owner every output-chain scenario uses; the
// recorded claims must come from a real manager-bound controller.
const scenarioManagerID = "telegram:main"

func newChainController(t *testing.T, h *subagentHarness) controllerapi.OutputQueueController {
	t.Helper()

	queue, ok := newTestController(h.mgr, &config.Config{}, nil, nil).
		ForManager(scenarioManagerID).(controllerapi.OutputQueueController)
	require.True(t, ok)
	require.NoError(t, queue.BindOutputDelivery(t.Context(), controllerapi.OutputBindingData{
		Driver: "telegram",
		Attributes: map[string]any{
			"bot_user_id": int64(1), "chat_id": int64(2), "topology": "group",
		},
	}))

	return queue
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

func installScenarioProcessService(t *testing.T, h *subagentHarness) backgroundprocess.Service {
	t.Helper()

	service := backgroundprocess.NewService(h.mgr.processStore, backgroundprocess.Options{
		OutputDir: t.TempDir(),
	})
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

func waitScenarioProcessState(
	t *testing.T,
	h *subagentHarness,
	processID string,
	want backgroundprocess.State,
) backgroundprocess.Process {
	t.Helper()

	var record backgroundprocess.Process
	require.Eventually(t, func() bool {
		current, err := h.mgr.processStore.GetProcess(context.Background(), processID)
		if err != nil {
			return false
		}

		record = current

		return current.State == want
	}, 5*time.Second, 10*time.Millisecond)

	return record
}

func assertModelDeliveries(
	t *testing.T,
	subscribers map[string]<-chan controllerapi.SessionNotification,
	wantOwner string,
	wantSessionID int64,
	wantMessage string,
) {
	t.Helper()

	var gotOwners []string
	for managerID, channel := range subscribers {
		select {
		case notification := <-channel:
			gotOwners = append(gotOwners, managerID)
			assert.Equal(t, wantSessionID, notification.SessionID)
			if wantMessage != "" {
				assert.Equal(t, wantMessage, notification.Notification.Message)
			}
		default:
		}
	}
	slices.Sort(gotOwners)

	wantOwners := []string(nil)
	if wantOwner != "" {
		wantOwners = []string{wantOwner}
	}
	assert.Equal(t, wantOwners, gotOwners)
}

// errSessionRead is the sentinel the fail-open publish test asserts against.
var errSessionRead = errors.New("session store unavailable")

// eventCollector accumulates everything a SubscribeAll channel yields, so a test
// can assert on the whole stream after the fact instead of racing it.
type eventCollector struct {
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

func requireSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()

	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for synchronization point")
	}
}

func collectEvents(ch <-chan controllerapi.SessionNotification) *eventCollector {
	c := &eventCollector{done: make(chan struct{}), changed: make(chan struct{}, 1)}

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

// newTestChild creates a root session and returns the ID of a subagent child of it.
func newTestChild(t *testing.T, mgr *svc, store *sessionstore.Store, workDir string) int64 {
	t.Helper()

	ctx := context.Background()
	pid := testProject(t, store, workDir)

	parent, err := mgr.store.CreateSession(ctx, pid, "fake-model", "", nil)
	require.NoError(t, err)

	childID, err := func() (int64, error) {
		var id int64
		err := mgr.store.(*sessionstore.Store).WithTx(ctx, func(tx *sql.Tx) error {
			var err error
			id, err = sessionstore.CreateSubagentSessionTx(
				ctx,
				tx,
				sessionstore.CreateSubagentSession{
					ProjectID:      pid,
					ParentID:       parent.ID,
					RootID:         parent.ID,
					AgentType:      "general",
					Model:          "fake-model",
					ReasoningLevel: "",
				},
			)
			return err
		})
		return id, err
	}()
	require.NoError(t, err)

	return childID
}

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

func interruptFirstRecovery(t *testing.T, dbPath string) int64 {
	t.Helper()
	secondCall := make(chan struct{})
	release := make(chan struct{})
	var calls int
	first := newSubagentHarnessOnDB(t, dbPath, func(_ string, _ []llmwire.Message) *llmwire.Response {
		calls++
		if calls == 1 {
			return &llmwire.Response{Text: "discarded before restart", FinishType: llmwire.FinishLength}
		}
		close(secondCall)
		<-release
		return &llmwire.Response{Text: "must be canceled", FinishType: llmwire.FinishStop}
	}, nil)

	first.startInboxWake()
	rootID, err := first.mgr.Send(first.ctx, first.projectID, "restart the recovery", "fake-model", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)
	waitForScenarioSignal(t, secondCall, "recovery call before restart")
	var recoveryRows int
	require.NoError(t, first.db.QueryRowContext(first.ctx, `SELECT COUNT(*) FROM messages
		WHERE session_id = ? AND retry_of_message_id IS NOT NULL`, rootID).Scan(&recoveryRows))
	require.Equal(t, 1, recoveryRows)
	first.shutdown()
	close(release)

	return rootID
}

func completeRecoveryAfterRestart(t *testing.T, dbPath string, rootID int64) {
	t.Helper()
	var resumedCalls int
	second := newSubagentHarnessOnDB(t, dbPath, func(_ string, messages []llmwire.Message) *llmwire.Response {
		resumedCalls++
		visible := scenarioTranscriptText(messages)
		require.Contains(t, visible, sessionstore.OutputLengthRecoveryPrompt)
		require.NotContains(t, visible, "discarded before restart")
		return &llmwire.Response{Text: "recovered after restart", FinishType: llmwire.FinishStop}
	}, nil)
	collector := collectEvents(second.mgr.bus.SubscribeAll())
	second.startInboxWake()
	second.mgr.resumeAfterRestart(second.ctx)
	waitForVisibleMessage(t, collector, rootID, "recovered after restart")
	drainScenarioClaims(t, "unused-response-restart.json", newChainController(t, second))
	waitForIdleAfterMessage(t, collector, rootID, "recovered after restart")
	// The recovery retry is a no-wake non-empty stop, so the two-phase check
	// spends one hidden candidate call and one confirming call.
	assert.Equal(t, 2, resumedCalls)
	second.shutdown()
	collector.stop()

	var unexpectedCalls atomic.Int64
	third := newSubagentHarnessOnDB(t, dbPath, func(_ string, _ []llmwire.Message) *llmwire.Response {
		unexpectedCalls.Add(1)
		return &llmwire.Response{Text: "unexpected rerun", FinishType: llmwire.FinishStop}
	}, nil)
	defer third.shutdown()
	third.startInboxWake()
	third.mgr.resumeAfterRestart(third.ctx)
	assert.Never(t, func() bool { return unexpectedCalls.Load() != 0 }, 300*time.Millisecond, 10*time.Millisecond)

	var rejectedRows, acceptedRows int
	require.NoError(t, third.db.QueryRowContext(third.ctx, `SELECT
		COUNT(*) FILTER (WHERE rejected_reason = 'output_length'),
		COUNT(*) FILTER (WHERE role = 'assistant' AND rejected_reason IS NULL AND finish_type = 'stop')
		FROM messages WHERE session_id = ?`, rootID).Scan(&rejectedRows, &acceptedRows))
	assert.Equal(t, 1, rejectedRows)
	// The candidate and its confirmation carry the same recovered text.
	assert.Equal(t, 2, acceptedRows)
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

func visibleEventMessages(events []controllerapi.SessionNotification, sessionID int64) []string {
	var messages []string
	for _, event := range events {
		if event.SessionID == sessionID && event.Notification.Message != "" {
			messages = append(messages, event.Notification.Message)
		}
	}

	return messages
}

func runIncompleteChildResponse(
	t *testing.T,
	childResponse *llmwire.Response,
) (*subagentHarness, subagent.Link) {
	t.Helper()
	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasUserContaining(messages, "CHILD_EMPTY_TOOL_FINISH") {
			return childResponse
		}
		if hasToolResultFor(messages, tool.IDTask) {
			return &llmwire.Response{Text: "parent handled incomplete child"}
		}

		return taskResponse("CHILD_EMPTY_TOOL_FINISH", "empty finish")
	}

	h := newSubagentHarnessWith(t, respond)
	t.Cleanup(h.shutdown)
	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "start empty-finish child", "fake-model", nil)
	require.NoError(t, err)
	link := h.waitForChildLink(parentID)
	h.waitForDelivery(link.ChildID)
	h.mgr.waitIdle(parentID)
	current, err := h.links.GetLink(h.ctx, link.ChildID)
	require.NoError(t, err)

	return h, *current
}

func taskResponse(prompt, description string) *llmwire.Response {
	return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
		ID: taskCallID, Name: tool.IDTask,
		Arguments: []byte(`{"prompt":"` + prompt + `","description":"` + description +
			`","subagent_type":"general"}`),
	}}}
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

func runnerCount(runners *runnerSet) int {
	return runnerLiveCount(runners)
}

func runnerCounts(runners *runnerSet) (int, int) {
	runners.mu.Lock()
	defer runners.mu.Unlock()

	return runners.running, runners.children
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

func registerTestRunner(ctx context.Context, manager *svc, rs *runner) (*runner, bool) {
	if !manager.runners.tryAdmit(rs.child, rs.parentID) {
		return nil, false
	}

	manager.liveMu.Lock()
	defer manager.liveMu.Unlock()

	manager.progress.SetLive(rs.sessionID, progressruntime.Live{Active: true})
	existing, registered := manager.runners.register(rs)
	if !registered {
		manager.runners.release(rs.child, rs.parentID)
		manager.updateLiveLocked(ctx, rs.sessionID)
	}

	return existing, registered
}

func transcriptOf(h *subagentHarness, sessionID int64) []llmwire.Message {
	messages, err := h.sessStore.LoadActiveMessages(h.ctx, sessionID)
	if err != nil {
		h.t.Fatalf("load transcript for session %d: %v", sessionID, err)
	}
	return toDTO(messages)
}

type stoppedRootScheduleCase struct {
	name, prompt, answer, trace string
	fresh                       bool
}

func runStoppedRootScheduleScenario(t *testing.T, tc stoppedRootScheduleCase) {
	t.Helper()
	started := make(chan struct{})
	release := make(chan struct{})
	h, rootID, collector := newStoppedRootScheduleHarness(t, tc, started, release)
	oldEpisode := time.Now().UTC().Add(-time.Hour)
	_, err := h.db.ExecContext(t.Context(),
		`UPDATE sessions SET episode_started_at = ? WHERE id = ?`, oldEpisode, rootID)
	require.NoError(t, err)
	deliveryID := runDueStoppedRootSchedule(t, h, rootID, collector, tc, started, release)
	successfulTrace := collector.snapshot()
	newEpisode := sessionEpisodeStart(t, h, rootID)
	assert.True(t, newEpisode.After(oldEpisode))

	assertStoppedRootScheduleResult(t, h, rootID, tc)
	assertStoppedRootScheduleDuplicate(t, h, rootID, deliveryID, tc, newEpisode)
	assertHarnessTrace(t, tc.trace, successfulTrace, rootID)
}

func newStoppedRootScheduleHarness(
	t *testing.T,
	tc stoppedRootScheduleCase,
	started chan<- struct{},
	release <-chan struct{},
) (*subagentHarness, int64, *eventCollector) {
	t.Helper()
	h := newSubagentHarnessWith(t, stoppedRootScheduleResponder(tc, started, release))
	t.Cleanup(h.shutdown)

	h.startInboxWake()
	rootID, err := h.mgr.Send(t.Context(), h.projectID, "initialize", "fake-model", map[string]any{
		controllerapi.SessionAttributeManagerID: "telegram-main",
	})
	require.NoError(t, err)
	h.mgr.waitIdle(rootID)
	require.NoError(t, h.mgr.sendToSession(t.Context(), rootID, "/stop"))
	collector := collectEvents(h.mgr.bus.SubscribeManager("telegram-main"))
	t.Cleanup(collector.stop)

	return h, rootID, collector
}

func scheduledTurnRequested(tc stoppedRootScheduleCase, messages []llmwire.Message) bool {
	if tc.fresh {
		return hasUserContaining(messages, tc.prompt)
	}

	return hasToolResultFor(messages, tool.IDSchedule)
}

func runDueStoppedRootSchedule(
	t *testing.T,
	h *subagentHarness,
	rootID int64,
	collector *eventCollector,
	tc stoppedRootScheduleCase,
	started <-chan struct{},
	release chan<- struct{},
) string {
	t.Helper()
	due := time.Now().Add(-time.Minute).UTC()
	entry, err := h.schedStore.AddSchedule(t.Context(), rootID, "", &due, tc.prompt, tc.fresh)
	require.NoError(t, err)

	observer := newScheduleRunningObserver(t, h.mgr.bus)
	sender := &orderedScheduleSender{SessionSender: h.mgr, running: observer.running, done: observer.done}
	executor := schedule.NewExecutor(h.schedStore, sender)
	executor.Start(t.Context())
	t.Cleanup(executor.Stop)
	requireSignal(t, started)
	assertStoppedRootActive(t, h, rootID)
	close(release)

	require.Eventually(t, func() bool {
		entries, listErr := h.schedStore.ListSchedules(t.Context(), rootID)
		return listErr == nil && len(entries) == 0 && !h.mgr.HasActiveLoop(rootID) &&
			lastAssistantTextDTO(h.parentMessages(rootID)) == tc.answer
	}, 5*time.Second, 10*time.Millisecond)
	executor.Stop()
	waitForVisibleMessage(t, collector, rootID, tc.answer)

	return fmt.Sprintf("schedule:one-shot:%d", entry.ID())
}

func assertStoppedRootActive(t *testing.T, h *subagentHarness, rootID int64) {
	t.Helper()
	rec, err := h.sessStore.GetSession(t.Context(), rootID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.SessionStatusActive, rec.Status)
}

func assertStoppedRootScheduleResult(t *testing.T, h *subagentHarness, rootID int64, tc stoppedRootScheduleCase) {
	t.Helper()
	messages := h.parentMessages(rootID)
	if tc.fresh {
		// The confirmed stop publishes one answer; its hidden candidate row
		// stays in the transcript with the same text.
		assert.Equal(t, 1, countMessageContentContaining(messages, tc.prompt))
		assert.Equal(t, 2, countMessageContentContaining(messages, tc.answer))

		return
	}

	assert.Equal(t, 1, countToolResultsFor(messages, tool.IDSchedule))
}

func assertStoppedRootScheduleDuplicate(
	t *testing.T,
	h *subagentHarness,
	rootID int64,
	deliveryID string,
	tc stoppedRootScheduleCase,
	episodeStartedAt time.Time,
) {
	t.Helper()
	require.NoError(t, h.mgr.sendToSession(t.Context(), rootID, "/stop"))
	applied, err := deliverStoppedRootSchedule(t, h.mgr, rootID, deliveryID, tc)
	require.NoError(t, err)
	assert.False(t, applied, "an acknowledged retry must not create another turn")
	require.Eventually(t, func() bool { return !h.mgr.HasActiveLoop(rootID) }, time.Second, 10*time.Millisecond)

	rec, err := h.sessStore.GetSession(t.Context(), rootID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.SessionStatusStopped, rec.Status)
	assert.Equal(t, episodeStartedAt, sessionEpisodeStart(t, h, rootID))
	assertStoppedRootScheduleResult(t, h, rootID, tc)
	_, err = enqueueCallResult(t.Context(), h.mgr.store, rootID, "missing-call", tool.IDSleep, "must stay stopped")
	require.NoError(t, err)
	assert.False(t, h.mgr.HasActiveLoop(rootID), "a late call result must not revive a stopped root")
	record, err := h.sessStore.GetSession(t.Context(), rootID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.SessionStatusStopped, record.Status)
}

func sessionEpisodeStart(t *testing.T, h *subagentHarness, rootID int64) time.Time {
	t.Helper()

	var startedAt time.Time
	require.NoError(t, h.db.QueryRowContext(t.Context(),
		`SELECT episode_started_at FROM sessions WHERE id = ?`, rootID).Scan(&startedAt))

	return startedAt
}

func deliverStoppedRootSchedule(
	t *testing.T,
	mgr *svc,
	rootID int64,
	deliveryID string,
	tc stoppedRootScheduleCase,
) (bool, error) {
	t.Helper()
	if tc.fresh {
		return enqueueScheduledInput(t.Context(), mgr.store, rootID, deliveryID, tc.prompt, true)
	}

	return enqueueScheduledInput(t.Context(), mgr.store, rootID, deliveryID, tc.prompt, false)
}

func createScheduleBoundarySubagent(t *testing.T, h *subagentHarness) int64 {
	t.Helper()

	parent, err := h.sessStore.CreateSession(t.Context(), h.projectID, "fake-model", "", nil)
	require.NoError(t, err)
	childID, err := func() (int64, error) {
		var id int64
		err := h.sessStore.WithTx(t.Context(), func(tx *sql.Tx) error {
			var err error
			id, err = sessionstore.CreateSubagentSessionTx(
				t.Context(),
				tx,
				sessionstore.CreateSubagentSession{
					ProjectID:      h.projectID,
					ParentID:       parent.ID,
					RootID:         parent.ID,
					AgentType:      "general",
					Model:          "fake-model",
					ReasoningLevel: "",
				},
			)
			return err
		})
		return id, err
	}()
	require.NoError(t, err)
	require.NoError(t, h.sessStore.UpdateSessionStatus(
		t.Context(), childID, sessionstore.SessionStatusCompleted,
	))

	return childID
}

func requireManagerNotification(
	t *testing.T,
	ch <-chan controllerapi.SessionNotification,
) controllerapi.SessionNotification {
	t.Helper()

	select {
	case notification := <-ch:
		return notification
	case <-time.After(time.Second):
		t.Fatal("manager notification timeout")
	}

	return controllerapi.SessionNotification{}
}

func requireNoManagerNotification(t *testing.T, ch <-chan controllerapi.SessionNotification) {
	t.Helper()

	select {
	case notification := <-ch:
		t.Fatalf("unexpected manager notification: %#v", notification)
	case <-time.After(20 * time.Millisecond):
	}
}

// effortRequest is what one provider call named: its model and reasoning effort.
type effortRequest struct {
	model  string
	effort string
}

// newEffortHarness is the daemon harness with real LLM clients pointed at a stub
// endpoint, and two models: one with no effort selector, one defaulting to "high".
func newEffortHarness(t *testing.T, baseURL string) *subagentHarness {
	t.Helper()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")

	db, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(ctx, db, dbPath))

	store := sessionstore.NewStore(db)
	sessStore := sessionstore.NewStore(db)
	links := subagent.NewStore(db, sessStore)
	schedStore := schedule.NewStore(db, sessStore)

	workDir := t.TempDir()
	cfg := &config.Config{WorkDir: workDir, Model: "plain-model", UnifiedConfig: &config.UnifiedConfig{
		Providers: map[string]config.ProviderEntry{
			"or": {Driver: "openrouter", APIKey: "key", BaseURL: baseURL},
		},
		Models: []config.ModelEntry{
			{ID: "plain-model", Provider: "or", ContextWindow: 200_000, MaxTokens: 64_000},
			{
				ID: "thinker", Provider: "or", ContextWindow: 200_000, MaxTokens: 64_000,
				EffortLevels:  []string{"low", "high"},
				DefaultEffort: "high",
				Reasoning:     &config.ReasoningSpec{Supported: true, Efforts: []string{"low", "high"}},
			},
		},
	}}

	factory := sessionbuild.BuildInput{Config: cfg, Store: sessStore, Resources: builtin.NewResources()}

	mgr, _ := newScenarioDaemon(
		context.Background(),
		factory,
		sessStore,
		links,
		budget.New(sessStore),
		schedule.NewService(schedStore, sessStore),
		func() string { return "plain-model" },
		db,
	)
	mgr.models = newModels(cfg)

	pid, err := store.GetOrCreateProject(ctx, workDir)
	require.NoError(t, err)

	return &subagentHarness{
		t: t, mgr: mgr, sessStore: sessStore, links: links, schedStore: schedStore,
		projectID: pid, ctx: ctx,
	}
}

// newModelAwareHarness is the subagent harness with an LLM client factory that
// mirrors llm.NewClient: an id outside the catalog cannot build a client.
func newModelAwareHarness(
	t *testing.T,
	known []string,
	respond func(system string, msgs []llmwire.Message) *llmwire.Response,
) *subagentHarness {
	t.Helper()

	return newModelAwareHarnessAtDB(t, filepath.Join(t.TempDir(), "test.db"), known, respond)
}

func newModelAwareHarnessAtDB(
	t *testing.T,
	dbPath string,
	known []string,
	respond func(string, []llmwire.Message) *llmwire.Response,
) *subagentHarness {
	t.Helper()

	ctx := context.Background()

	db, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(ctx, db, dbPath))

	store := sessionstore.NewStore(db)
	sessStore := sessionstore.NewStore(db)
	links := subagent.NewStore(db, sessStore)
	schedStore := schedule.NewStore(db, sessStore)

	workDir := t.TempDir()
	cfg := &config.Config{WorkDir: workDir, Model: known[0]}
	cfg.UnifiedConfig = &config.UnifiedConfig{}
	for _, model := range known {
		cfg.UnifiedConfig.Models = append(cfg.UnifiedConfig.Models, config.ModelEntry{ID: model, ContextWindow: 200000})
	}

	factory := scriptedBuildInput(t, cfg, sessStore, nil, func(c *config.Config) (llm.Client, error) {
		if !slices.Contains(known, c.Model) {
			return nil, fmt.Errorf("model %q not found in config", c.Model)
		}

		return &scriptedLLM{respond: respond}, nil
	})

	mgr, _ := newScenarioDaemon(
		context.Background(),
		factory,
		sessStore,
		links,
		budget.New(sessStore),
		schedule.NewService(schedStore, sessStore),
		func() string { return known[0] },
		db,
	)

	pid, err := store.GetOrCreateProject(ctx, workDir)
	require.NoError(t, err)

	return &subagentHarness{
		t: t, mgr: mgr, sessStore: sessStore, links: links, schedStore: schedStore,
		projectID: pid, ctx: ctx,
	}
}

func (h *subagentHarness) liveSession(sessionID int64) *session.Session {
	rs, ok := h.mgr.runners.load(sessionID)

	if !ok {
		return nil
	}

	return rs.Service()
}

func countAssistantReplies(msgs []llmwire.Message) int {
	count := 0

	for _, m := range msgs {
		if m.Role == llmwire.RoleAssistant && m.Content != "" {
			count++
		}
	}

	return count
}

func hasSessionErrorNotice(events []controllerapi.SessionNotification) bool {
	for _, e := range events {
		if e.Notification.Type == sessionevent.NotifyMessage &&
			strings.Contains(e.Notification.Message, "Session error") {
			return true
		}
	}

	return false
}

// skillScenarioSkill is the project-local user-invocable skill the scenario
// activates through both the /skill command and the model skill tool.
const skillScenarioSkill = `---
name: review
description: Review the current change
---
Review the change carefully.
`

func skillScenarioHasEnvelope(msgs []llmwire.Message) bool {
	for _, m := range msgs {
		if m.Role == llmwire.RoleUser && strings.Contains(m.Content, "<name>review</name>") {
			return true
		}
	}

	return false
}

// newSkillScenarioHarness is the standard daemon harness over a project
// directory carrying one user-invocable project skill.
func newSkillScenarioHarness(
	t *testing.T,
	respond func(system string, msgs []llmwire.Message) *llmwire.Response,
) *subagentHarness {
	t.Helper()

	h := newSubagentHarnessOnDBWithProjectConfig(
		t, filepath.Join(t.TempDir(), "test.db"), respond, nil, nil,
	)

	var workDir string
	require.NoError(t, h.db.QueryRow(
		`SELECT work_dir FROM projects WHERE id = ?`, h.projectID,
	).Scan(&workDir))

	skillsDir := filepath.Join(workDir, ".claude", "skills", "review")
	require.NoError(t, os.MkdirAll(skillsDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(skillsDir, "SKILL.md"), []byte(skillScenarioSkill), 0o600,
	))

	return h
}

func traceClaims(t *testing.T, name string) []harnessTraceClaim {
	t.Helper()

	data, err := os.ReadFile(harnessTracePath(name))
	require.NoError(t, err)

	var file harnessTraceFile
	require.NoError(t, json.Unmarshal(data, &file))

	return file.Claims
}

func inputIDOf(t *testing.T, h *subagentHarness, root int64) int64 {
	t.Helper()

	var inputID int64
	require.NoError(t, h.db.QueryRow(`SELECT id FROM session_inbox
		WHERE session_id = ? AND raw_content = '/skill review'`, root).Scan(&inputID))

	return inputID
}

// skillCompactRespond answers summarization prompts with a brief and every other
// turn with text, so each send reaches idle in one turn.
func skillCompactRespond(_ string, msgs []llmwire.Message) *llmwire.Response {
	if isCompactionInstruction(msgs) {
		return &llmwire.Response{
			Text: "## Goal\nfollow the playbook\n## Progress\n- read it\n## Context for Continuation\ncarry on",
		}
	}

	return &llmwire.Response{Text: "work done"}
}

// skillRecorder captures what every scripted provider call was handed — the
// system prompt and the transcript — so a test can assert on the request the
// model actually received rather than on the stored transcript alone.
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

// newSkillHarness is the subagent harness over a project WorkDir carrying
// .claude/skills. Skills load when a session is created, so writing them before
// the first Send is enough.
func newSkillHarness(
	t *testing.T,
	skills map[string]string,
	respond func(string, []llmwire.Message) *llmwire.Response,
) (*subagentHarness, *skillRecorder) {
	t.Helper()

	rec := &skillRecorder{}
	h := newSubagentHarnessWith(t, rec.wrap(respond))

	for name, body := range skills {
		dir := filepath.Join(h.workDir(), ".claude", "skills", name)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o600))
	}

	return h, rec
}

func (h *subagentHarness) workDir() string {
	h.t.Helper()

	workDir, err := h.mgr.store.GetProjectWorkDir(h.ctx, h.projectID)
	require.NoError(h.t, err)

	return workDir
}

// plainRespond answers every turn with text, so a session reaches idle in one turn.
func plainRespond(_ string, _ []llmwire.Message) *llmwire.Response {
	return &llmwire.Response{Text: "work done"}
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

// countMessagesWithSkill counts transcript rows carrying the named skill envelope.
func countMessagesWithSkill(msgs []llmwire.Message, name string) int {
	count := 0

	for _, m := range msgs {
		count += strings.Count(m.Content, "<name>"+name+"</name>")
	}

	return count
}

// warningNotices returns every ⚠️ line a controller saw for the session.
func warningNotices(events []controllerapi.SessionNotification, sessionID int64) []string {
	var out []string

	for _, event := range events {
		if event.SessionID != sessionID || event.Notification.Type != sessionevent.NotifyMessage {
			continue
		}

		if strings.HasPrefix(event.Notification.Message, "⚠️ ") {
			out = append(out, event.Notification.Message)
		}
	}

	return out
}

func (h *subagentHarness) requireInboxDrained(sessionID int64) {
	h.t.Helper()

	_, err := h.sessStore.PeekPending(h.ctx, sessionID)
	require.ErrorIs(h.t, err, sessionstore.ErrNoPendingInput, "the rejected input must leave the inbox")
}

// newSpawnEffortHarness is the daemon harness with real LLM clients pointed at a
// stub endpoint, and two reasoning models whose effort vocabularies overlap only
// on "low" — so an inherited level is distinguishable from a settled one.
func newSpawnEffortHarness(t *testing.T, baseURL string) *subagentHarness {
	t.Helper()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")

	db, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(ctx, db, dbPath))

	store := sessionstore.NewStore(db)
	sessStore := sessionstore.NewStore(db)
	links := subagent.NewStore(db, sessStore)
	schedStore := schedule.NewStore(db, sessStore)

	workDir := t.TempDir()
	cfg := &config.Config{WorkDir: workDir, Model: "parent-model", UnifiedConfig: &config.UnifiedConfig{
		Providers: map[string]config.ProviderEntry{
			"or": {Driver: "openrouter", APIKey: "key", BaseURL: baseURL},
		},
		Models: []config.ModelEntry{
			reasoningModelEntry("parent-model", []string{"low", "high"}, "high"),
			reasoningModelEntry("child-model", []string{"low", "medium"}, "low"),
		},
	}}

	factory := sessionbuild.BuildInput{Config: cfg, Store: sessStore}

	mgr, _ := newScenarioDaemon(
		context.Background(),
		factory,
		sessStore,
		links,
		budget.New(sessStore),
		schedule.NewService(schedStore, sessStore),
		func() string { return "parent-model" },
		db,
	)
	mgr.models = newModels(cfg)

	pid, err := store.GetOrCreateProject(ctx, workDir)
	require.NoError(t, err)

	return &subagentHarness{
		t: t, mgr: mgr, sessStore: sessStore, links: links, schedStore: schedStore,
		projectID: pid, ctx: ctx,
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

// The status header a controller receives, duplicated from internal/session on
// purpose: it is the contract with the human, not an implementation detail.
const statusReportHeader = "## Session progress"

func statusReports(events []controllerapi.SessionNotification, sessionID int64) []string {
	var out []string

	for _, event := range events {
		if event.SessionID != sessionID || event.Notification.Type != sessionevent.NotifyMessage {
			continue
		}

		if strings.HasPrefix(event.Notification.Message, statusReportHeader) {
			out = append(out, event.Notification.Message)
		}
	}

	return out
}

// todoRows extracts the icon-only TODO rows from a rendered status report.
func todoRows(report string) []string {
	var rows []string

	for line := range strings.SplitSeq(report, "\n") {
		for _, icon := range []string{"⏳", "🔄", "✅", "🚫", "❔"} {
			if strings.HasPrefix(line, "  - "+icon+" ") {
				rows = append(rows, line)
			}
		}
	}

	return rows
}

func assistantTexts(messages []llmwire.Message) []string {
	var texts []string
	for _, message := range messages {
		if message.Role == llmwire.RoleAssistant && message.Content != "" {
			texts = append(texts, message.Content)
		}
	}
	return texts
}

func lastIdleStatus(events []controllerapi.SessionNotification, sessionID int64) *sessionevent.Notification {
	for _, event := range slices.Backward(events) {
		if event.SessionID == sessionID && event.Notification.Type == sessionevent.NotifyStateChanged &&
			event.Notification.Status == controllerapi.StateIdle {
			n := event.Notification
			return &n
		}
	}

	return nil
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

// seedChildCandidateConfirm drives the child through the two-phase check at
// the store level: a hidden candidate plus its host nudge, then a confirming
// (ack) stop. It returns the candidate's transcript id.
func seedChildCandidateConfirm(t *testing.T, h *subagentHarness, childID int64) int64 {
	t.Helper()
	ctx := h.ctx

	stored := func(role, content string) *transcript.Message {
		return &transcript.Message{Role: role, Content: content}
	}

	iteration := 1
	_, err := h.sessStore.Commit(
		ctx,
		sessionstore.Commit{
			SessionID: childID,
			Messages: []*transcript.Message{
				stored("assistant", "the full child answer"),
				stored("user", "[AUTOMATED CHECK] second look"),
			},
			State: sessionstore.StatePatch{
				Iteration: &iteration,
				Candidate: &sessionstore.CandidateChange{NextRef: 0},
			},
		},
	)
	require.NoError(t, err)

	state, err := h.sessStore.LoadCompletionCheckState(ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, state.CandidateID)
	candidateID := *state.CandidateID

	iteration = 2
	_, err = h.sessStore.Commit(
		ctx,
		sessionstore.Commit{
			SessionID: childID,
			Messages:  []*transcript.Message{stored("assistant", "why I am stopping")},
			State: sessionstore.StatePatch{
				Iteration:         &iteration,
				ConfirmedAnswerID: &candidateID,
				Candidate:         &sessionstore.CandidateChange{Expected: candidateID, NextRef: -1},
			},
		},
	)
	require.NoError(t, err)

	require.NoError(t, h.sessStore.UpdateSessionStatus(ctx, childID, sessionstore.SessionStatusCompleted))

	var pointer int64
	require.NoError(t, h.db.QueryRowContext(ctx,
		`SELECT COALESCE(completion_check_confirmed_answer_id, 0) FROM sessions WHERE id = ?`,
		childID).Scan(&pointer))
	require.Equal(t, candidateID, pointer, "confirm set the durable answer pointer")

	return candidateID
}

func createBackgroundChild(t *testing.T, mgr *svc, projectID, parentID int64) int64 {
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

func assertProcessInputDoesNotRearmAfterStop(t *testing.T, stopChild bool) {
	t.Helper()

	ctx := context.Background()
	mgr, _, projects := newTestManager(t)
	projectID := testProject(t, projects, "/tmp/process-stop-rearm")
	root, err := mgr.store.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)
	childID := createBackgroundChild(t, mgr, projectID, root.ID)

	require.NoError(
		t,
		seedTerminalChild(ctx, projects, childID, subagent.StateCompleted, "first outcome", subagent.OutcomeCompleted),
	)
	require.NoError(t, mgr.store.UpdateSessionStatus(ctx, childID, sessionstore.SessionStatusCompleted))
	link, err := mgr.links.GetLink(ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, link)
	won, err := mgr.links.DeliverBackgroundCompletion(ctx, *link, 1)
	require.NoError(t, err)
	require.True(t, won)
	_, err = mgr.store.Enqueue(
		ctx,
		sessionstore.Input{
			SessionID:  childID,
			Source:     sessionstore.InputSourceProcess,
			Content:    "<process_completion>late process</process_completion>",
			Attributes: map[string]any{"process_id": "late-process"},
		},
	)
	require.NoError(t, err)

	unlock, err := mgr.lockSessionTree(ctx, root.ID)
	require.NoError(t, err)
	ready := make(chan error, 1)
	go func() { ready <- mgr.inputReady(ctx, childID) }()
	stopID := root.ID
	if stopChild {
		stopID = childID
	}
	require.NoError(t, mgr.store.UpdateSessionStatus(ctx, stopID, sessionstore.SessionStatusStopped))
	unlock()
	require.NoError(t, <-ready)

	link, err = mgr.links.GetLink(ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.Equal(t, subagent.StateCompleted, link.State)
	assert.Equal(t, int64(1), link.ActivationSeq)
	pending, err := mgr.store.PeekPending(ctx, childID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.InputSourceProcess, pending.Source)
}

func containsAll(value string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(value, needle) {
			return false
		}
	}
	return true
}

// errDeliveryCrash stands in for the process dying between a child's terminal
// mark and the commit of its completion into the parent transcript.
var errDeliveryCrash = errors.New("daemon died before the completion was committed")

// The delivery transaction must fail before either the input or its acknowledgment commits.
type crashGateTransactions struct {
	subagent.Store

	once     sync.Once
	rejected chan struct{}
}

func newCrashGate(inner subagent.Store) *crashGateTransactions {
	return &crashGateTransactions{Store: inner, rejected: make(chan struct{})}
}

func (g *crashGateTransactions) DeliverCompletion(context.Context, subagent.Link, string) (bool, error) {
	g.once.Do(func() { close(g.rejected) })

	return false, errDeliveryCrash
}

func (g *crashGateTransactions) DeliverBackgroundCompletion(context.Context, subagent.Link, int) (bool, error) {
	g.once.Do(func() { close(g.rejected) })

	return false, errDeliveryCrash
}

// crashWindowRespond drives a parent that spawns exactly one child and answers
// once its completion arrives, in either delivery shape.
func crashWindowRespond(background bool) func(string, []llmwire.Message) *llmwire.Response {
	args := `{"prompt":"CHILD_TASK do it","description":"c","subagent_type":"general"}`
	if background {
		args = `{"prompt":"CHILD_TASK do it","description":"c","subagent_type":"general","background":true}`
	}

	return func(_ string, msgs []llmwire.Message) *llmwire.Response {
		switch {
		case hasUserContaining(msgs, "CHILD_TASK"):
			return &llmwire.Response{Text: "child finished: 42"}
		case hasUserContaining(msgs, "<subagent_completion>"):
			return &llmwire.Response{Text: "parent got the child result"}
		case hasToolResultFor(msgs, "task") && background:
			return &llmwire.Response{Text: "child launched"}
		case hasToolResultFor(msgs, "task"):
			return &llmwire.Response{Text: "parent got the child result"}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID: taskCallID, Name: "task", Arguments: []byte(args),
		}}}
	}
}

func containsMessage(
	events []controllerapi.SessionNotification,
	sessionID int64,
	message string,
) bool {
	for _, event := range events {
		if event.SessionID == sessionID && event.Notification.Message == message {
			return true
		}
	}

	return false
}

func containsState(
	events []controllerapi.SessionNotification,
	sessionID int64,
	status sessionevent.State,
) bool {
	for _, event := range events {
		if event.SessionID == sessionID && event.Notification.Status == status {
			return true
		}
	}

	return false
}

func containsStateWithReason(
	events []controllerapi.SessionNotification,
	sessionID int64,
	status sessionevent.State,
	reason string,
) bool {
	for _, event := range events {
		if event.SessionID == sessionID && event.Notification.Status == status &&
			event.Notification.Reason == reason {
			return true
		}
	}

	return false
}

type wakeObserver struct {
	Store
	sessionID int64
	observed  chan struct{}
	once      sync.Once
}

func (s *wakeObserver) GetSession(ctx context.Context, id int64) (*sessionstore.SessionRecord, error) {
	record, err := s.Store.GetSession(ctx, id)
	if id == s.sessionID {
		s.once.Do(func() { close(s.observed) })
	}
	return record, err
}
