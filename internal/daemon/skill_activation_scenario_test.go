package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionevent"
)

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

// The user-visible skill-activation receipt closes the replaceable progress
// chain: one persistent receipt per activation, ordered after the accepted
// input and before any later progress reuses the pre-activation card.
func TestHarnessScenario_SkillActivationReceiptOrdersTheOutputChain(t *testing.T) {
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasToolResultFor(msgs, "skill") {
			return &llmwire.Response{Text: "model activation complete"}
		}

		if hasUserContaining(msgs, "invoke the skill yourself") {
			return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
				ID: "receipt-skill", Name: "skill", Arguments: []byte(`{"name":"review"}`),
			}}}
		}

		if skillScenarioHasEnvelope(msgs) || hasToolResultFor(msgs, "ls") {
			return &llmwire.Response{Text: "probe answer"}
		}

		return &llmwire.Response{
			Text: "Running the first probe",
			ToolCalls: []llmwire.ToolCall{{
				ID: "receipt-ls", Name: "ls", Arguments: []byte(`{"path":"."}`),
			}},
		}
	}

	h := newSkillScenarioHarness(t, respond)
	defer h.shutdown()

	collector := collectEvents(h.mgr.PubSub().SubscribeAll())
	defer collector.stop()

	root, err := h.mgr.Send(h.ctx, h.projectID, "start the skill scenario", "fake-model", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)
	waitForVisibleMessage(t, collector, root, "probe answer")
	// Settle before the next input: sending into a live loop keeps the same
	// runner and races the session re-creation the golden trace records.
	h.waitUntil("first turn settled", func() bool { return !h.mgr.HasActiveLoop(root) })

	// Explicit /skill activation through the durable input boundary.
	require.NoError(t, h.mgr.SendToSession(h.ctx, root, "/skill review"))
	waitForVisibleMessage(t, collector, root, "🔧 Activated skill: review")
	waitForVisibleMessage(t, collector, root, "probe answer")

	// Model-initiated activation through the skill tool.
	require.NoError(t, h.mgr.SendToSession(h.ctx, root, "invoke the skill yourself"))
	waitForVisibleMessage(t, collector, root, "model activation complete")

	controller := newChainController(t, h)
	drainScenarioClaims(t, "skill_activation_receipt.json", controller)
	waitForIdleAfterMessage(t, collector, root, "model activation complete")

	assertHarnessTrace(t, "skill_activation_receipt.json", collector.snapshot(), root)

	// Exactly one receipt row per activation path, each a persistent message
	// carrying the canonical skill name and nothing else.
	var receipts []struct {
		ID      int64
		Type    string
		Content string
	}
	rows, err := h.db.Query(`SELECT id, type, content FROM session_outbox
		WHERE session_id = ? AND content LIKE '🔧 Activated skill: %' ORDER BY id`, root)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var row struct {
			ID      int64
			Type    string
			Content string
		}
		require.NoError(t, rows.Scan(&row.ID, &row.Type, &row.Content))
		receipts = append(receipts, row)
	}
	require.NoError(t, rows.Err())
	require.Len(t, receipts, 2, "one receipt per activation path")
	for _, row := range receipts {
		assert.Equal(t, "message_persistent", row.Type)
		assert.Equal(t, "🔧 Activated skill: review", row.Content)
	}

	// A replaceable progress output exists before the first receipt and later
	// progress starts after it: the persistent receipt closed the chain.
	var progressBefore int
	err = h.db.QueryRow(`SELECT COUNT(*) FROM session_outbox
		WHERE session_id = ? AND type = 'message_replaceable' AND id < ?`,
		root, receipts[0].ID).Scan(&progressBefore)
	require.NoError(t, err)
	assert.Positive(t, progressBefore, "a replaceable card existed before the receipt")

	// The receipt's source key binds it to its accepted input, so a promotion
	// replay cannot mint a second receipt, and the recorded claim chain proves
	// the receipt was published as its own persistent message.
	var receiptClaim int
	for _, claim := range traceClaims(t, "skill_activation_receipt.json") {
		if claim.SourceKey == fmt.Sprintf("input:%d:skill_receipt", inputIDOf(t, h, root)) {
			receiptClaim++
		}
	}
	assert.Equal(t, 1, receiptClaim, "the receipt is claimed exactly once by its input key")

	// The explicit /skill activation receipt reached the controller sink as a
	// message event exactly once (the model-tool receipt rides the direct
	// output of its tool claim, not a second standalone event), and no skill
	// content reaches the manager.
	trace := collector.snapshot()
	assert.Equal(t, 1, countPublishedMessage(trace, root, "🔧 Activated skill: review"))
	for _, event := range trace {
		if event.SessionID != root || event.Notification.Type != sessionevent.NotifyMessage {
			continue
		}

		assert.NotContains(t, event.Notification.Message, "Review the change carefully.",
			"no skill content reaches the manager")
	}
}

// newSkillScenarioHarness is the standard daemon harness over a project
// directory carrying one user-invocable project skill.
func newSkillScenarioHarness(
	t *testing.T,
	respond func(system string, msgs []llmwire.Message) *llmwire.Response,
) *subagentHarness {
	t.Helper()

	h := newSubagentHarnessOnDBWithProjectConfig(
		t, filepath.Join(t.TempDir(), "test.db"), respond, nil, false, nil,
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
