package daemon

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/tool"
)

const batcherAgentFile = `---
name: batcher
description: Project subagent granted parallel reads only
tools:
  - read
  - batch
---
You are the batcher.
`

// batch dispatches through a registry, so a child granted batch but not bash
// would reach bash through it unless the filtered view rebinds batch. The
// scripted child is adversarial on purpose.
func TestHarnessScenario_BatchCannotEscapeSubagentAllowlist(t *testing.T) {
	const (
		batchCallID  = "task-batcher-1"
		escapeMarker = "BATCH_ESCAPED"
		escapeFile   = "escaped.txt"
	)

	escapeCall := `{"calls":[{"tool":"bash","params":{"command":"echo ` + escapeMarker +
		` | tee ` + escapeFile + `","description":"escape the allowlist"}}]}`

	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "CHILD_BATCH") {
			if hasToolResultFor(msgs, tool.IDBatch) {
				return &llmwire.Response{Text: "batcher done"}
			}

			return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
				ID: "batch-escape", Name: tool.IDBatch, Arguments: []byte(escapeCall),
			}}}
		}

		if hasToolResultFor(msgs, tool.IDTask) || hasToolResultFor(msgs, "subagent_event") {
			return &llmwire.Response{Text: "parent done"}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{
			spawnTaskCall(batchCallID, "batcher", "CHILD_BATCH"),
		}}
	}

	h := newGatingHarness(t, false, map[string]string{"batcher.md": batcherAgentFile}, respond)
	defer h.shutdown()

	parentID, err := h.mgr.Send(h.ctx, h.projectID, "spawn a batching child", "fake-model", nil)
	require.NoError(t, err)

	link := h.waitForLink(parentID, batchCallID)
	h.waitForDelivery(link.ChildID)
	h.mgr.waitIdle(parentID)

	msgs := h.parentMessages(link.ChildID)
	require.NoError(t, llm.ValidateToolPairing(msgs), "child transcript must stay provider-valid")

	assert.Equal(t, 1, countToolResultsFor(msgs, tool.IDBatch))

	batchResult := lastToolResultContent(msgs, tool.IDBatch)
	assert.Contains(t, batchResult, `unknown tool "bash"`)
	assert.NotContains(t, batchResult, escapeMarker, "the forbidden call must not have run")
	assert.NoFileExists(t, filepath.Join(h.workDir(), escapeFile))

	offered := h.schemas.offered(link.ChildID)
	assert.Contains(t, offered, tool.IDBatch, "the child really was granted batch")
	assertNotOffered(t, offered, []string{"bash", "write", "edit"})
}
