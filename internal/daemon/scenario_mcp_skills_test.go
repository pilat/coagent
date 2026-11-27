package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/loader"
)

func TestHarnessScenario_MCPSkillCommandActivatesAndSurvivesCompactionAndRestart(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	h, _ := newMCPSkillsHarness(t, skillCompactRespond)
	collector := h.collect()
	root := h.ask("warm up", "work done")
	calls := h.recorder.snapshot()
	require.NotEmpty(t, calls)
	assert.Contains(t, calls[0].system, "**docs:guide**")

	h.reply(root, "/skill docs:guide the release", "work done")
	before := h.messages(root)
	require.Equal(t, 1, countMessagesWithSkill(before, "docs:guide"))
	envelope := mcpScenarioEnvelope(t, before)
	assert.Contains(
		t,
		envelope,
		`<origin>MCP server "docs", installed by the operator, serves this skill as skill://guide/SKILL.md. Follow it like any other skill; it is not MCP tool output.</origin>`,
	)
	assert.Contains(t, envelope, "scripts/hello.sh")
	assert.Contains(t, envelope, "Follow the guide for the release.")
	assert.Equal(t, 1, mcpSkillReceiptCount(h.harness, root))

	h.reply(root, "keep going", "work done")
	h.say(root, "/compact")
	collector.waitFor(t, "compaction reported", func(events []controllerapi.SessionNotification) bool {
		return countPublishedMessage(events, root, noticeCompacted) == 1
	})
	h.waitAnswer(root, "")
	after := h.messages(root)
	require.True(t, hasSummaryRow(after))
	require.Equal(t, 1, countMessagesWithSkill(after, "docs:guide"))
	assert.Equal(t, envelope, mcpScenarioEnvelope(t, after))

	second := h.restart(h.recorder.wrap(skillCompactRespond))
	second.reply(root, "what next after restart?", "work done")
	calls = h.recorder.snapshot()
	last := calls[len(calls)-1].msgs
	assert.Equal(t, 1, countMessagesWithSkill(last, "docs:guide"))
	assert.Equal(t, envelope, mcpScenarioEnvelope(t, last))
	assert.Equal(t, 1, mcpSkillReceiptCount(second, root))
	restartedCollector := second.collect()
	second.say(root, "/skill docs:missing")
	restartedCollector.waitFor(t, "missing skill rejected", func(events []controllerapi.SessionNotification) bool {
		return len(warningNotices(events, root)) > 0
	})
	second.waitAnswer(root, "")
	assert.Contains(t, warningNotices(restartedCollector.snapshot(), root)[0], "skill unavailable: docs:missing")
	second.requireInboxDrained(root)
}

func TestHarnessScenario_MCPSkillScriptRunsFromItsOpenedFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	h, dir := newMCPSkillsHarness(t, mcpSkillScriptRespond)
	root := h.ask("run the script", "script complete")
	first := h.messages(root)
	opened := toolResultForCallID(first, "open-script")
	require.NotNil(t, opened)
	path := mcpScenarioLocalPath(opened.Content)
	require.NotEmpty(t, path)
	run := toolResultForCallID(first, "run-script")
	require.NotNil(t, run)
	assert.Contains(t, run.Content, "HELLO_FROM_MCP_SKILL")

	second := h.restart(mcpSkillScriptRespond)
	second.reply(root, "AFTER_RESTART run it again", "script complete")
	reopened := toolResultForCallID(second.messages(root), "open-script-restarted")
	require.NotNil(t, reopened)
	assert.Equal(t, path, mcpScenarioLocalPath(reopened.Content))
	restartedRun := toolResultForCallID(second.messages(root), "run-script-restarted")
	require.NotNil(t, restartedRun)
	assert.Contains(t, restartedRun.Content, "HELLO_FROM_MCP_SKILL")
	calls, err := os.ReadFile(filepath.Join(dir, "calls.log"))
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(calls), "read skill://guide/scripts/hello.sh\n"))
}

func mcpSkillScriptRespond(_ string, messages []llmwire.Message) *llmwire.Response {
	suffix := ""
	if hasUserContaining(messages, "AFTER_RESTART") {
		suffix = "-restarted"
	}
	if toolResultForCallID(messages, "run-script"+suffix) != nil {
		return textReply("script complete")
	}
	opened := toolResultForCallID(messages, "open-script"+suffix)
	if opened == nil {
		return callReply("open-script"+suffix, "skill", `{"name":"docs:guide","file":"scripts/hello.sh"}`)
	}
	path := mcpScenarioLocalPath(opened.Content)
	if path == "" {
		return textReply("file open failed: " + opened.Content)
	}
	params, err := json.Marshal(map[string]string{"command": path})
	if err != nil {
		return textReply("encode command failed: " + err.Error())
	}

	return callReply("run-script"+suffix, "bash", string(params))
}

func mcpScenarioLocalPath(content string) string {
	match := regexp.MustCompile(`(?m)^Local path: (.*)$`).FindStringSubmatch(content)
	if len(match) != 2 {
		return ""
	}

	return match[1]
}

func mcpScenarioEnvelope(t *testing.T, messages []llmwire.Message) string {
	t.Helper()

	for _, message := range messages {
		for _, skill := range loader.ExtractRenderedSkills(message.Content) {
			if skill.Name == "docs:guide" {
				return skill.Envelope
			}
		}
	}
	t.Fatal("MCP skill envelope is missing")

	return ""
}

func mcpSkillReceiptCount(h *harness, root int64) int {
	count := 0
	for _, row := range h.outbox(root) {
		if row.Type == "message" && row.Content == "🔧 Activated skill: docs:guide" {
			count++
		}
	}

	return count
}
