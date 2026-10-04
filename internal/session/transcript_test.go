package session

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionprompt"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/transcript"
)

// TestAttachments_SurviveRestart is the append→restart→reload protocol case:
// refs persisted on role-tool rows must come back byte-identical in order.
func TestAttachments_SurviveRestart(t *testing.T) {
	ctx := context.Background()
	_, store, sessionID := newAttachmentsStore(t)
	ms := newMessageStore(store, sessionID)

	appendImageToolResult(ctx, t, ms, "call-1")

	reloaded := newMessageStore(store, sessionID)
	require.NoError(t, reloaded.reloadMessages(ctx))

	got := reloaded.getMessages()
	require.Len(t, got, 2)
	require.Equal(t, llmwire.RoleTool, got[1].Role)
	assert.Equal(t, demoRefs, got[1].Images, "refs must survive restart in original order")
	assert.Empty(t, got[0].Images, "non-tool rows carry no refs")
}

func TestEstimateTokens_ImageDeltaBounded(t *testing.T) {
	base := []llmwire.Message{
		{Role: llmwire.RoleUser, Content: strings.Repeat("x", 400)},
	}
	withRef := func(size int64) []llmwire.Message {
		return []llmwire.Message{
			base[0],
			{
				Role: llmwire.RoleTool, Content: "img", ToolName: "read",
				Images: []llmwire.ImageRef{{Path: "/tmp/a.png", Mime: llmwire.MimeImagePng, Size: size}},
			},
		}
	}

	assert.Equal(t, 0, estimateTokens(withRef(0))-estimateTokens(base), "Size=0 is legal and free-ish")
	assert.Equal(t, 1, estimateTokens(withRef(5))-estimateTokens(base), "small Size charges Size/4")

	delta := estimateTokens(withRef(1<<30)) - estimateTokens(base)
	assert.Equal(t, 8192, delta, "huge files cap at the ceiling instead of triggering spurious compaction")
}

// A fresh session, a resume from SQLite and a subagent all start with nothing
// measured, so the trigger runs on the whole-transcript estimate.
func TestProjectContextSize_UnmeasuredSessionsEstimate(t *testing.T) {
	agent := newTestAgent()
	setTestMessages(agent, buildMessagesWithTokens(1000))

	size, estimated := agent.projectContextSize()

	assert.True(t, estimated)
	assert.Equal(t, 1000+agent.requestOverhead(), size)
}

func TestHasPendingExternalCall(t *testing.T) {
	tests := []struct {
		name   string
		staged map[string]string
		msgs   []llmwire.Message
		want   bool
	}{
		{
			name:   "staged and unanswered",
			staged: map[string]string{"c1": tool.IDConfigEdit},
			msgs:   []llmwire.Message{asst("", call("c1", tool.IDConfigEdit))},
			want:   true,
		},
		{
			name:   "staged and answered",
			staged: map[string]string{"c1": tool.IDConfigEdit},
			msgs:   []llmwire.Message{asst("", call("c1", tool.IDConfigEdit)), toolRes("c1")},
			want:   false,
		},
		{
			name:   "nothing staged",
			staged: nil,
			msgs:   []llmwire.Message{asst("", call("c1", tool.IDConfigEdit))},
			want:   false,
		},
		{
			name:   "staged in a superseded turn",
			staged: map[string]string{"c1": tool.IDConfigEdit},
			msgs: []llmwire.Message{
				asst("", call("c1", tool.IDConfigEdit)),
				toolRes("c1"),
				usr("next"),
				asst("", call("c2", "read")),
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := newTestAgent()
			agent.stagedCalls = tt.staged
			setTestMessages(agent, tt.msgs)

			assert.Equal(t, tt.want, agent.HasPendingExternalCall())
		})
	}
}

// Repair protection is the wider set: an external call is protected by name even
// before the daemon has staged anything, and past a trailing user message.
func TestPendingExternalCallIDs(t *testing.T) {
	tests := []struct {
		name   string
		staged map[string]string
		msgs   []llmwire.Message
		want   []string
	}{
		{
			name: "sleep is protected, as it always was",
			msgs: []llmwire.Message{asst("", call("c1", tool.IDSleep))},
			want: []string{"c1"},
		},
		{
			name: "a config call is protected by name",
			msgs: []llmwire.Message{asst("", call("c1", tool.IDConfigEdit))},
			want: []string{"c1"},
		},
		{
			name: "an ordinary call is not",
			msgs: []llmwire.Message{asst("", call("c1", "read"))},
			want: nil,
		},
		{
			name: "protection survives a trailing user message",
			msgs: []llmwire.Message{asst("", call("c1", tool.IDConfigEdit)), usr("hurry up")},
			want: []string{"c1"},
		},
		{
			name: "mixed turn protects only the external half",
			msgs: []llmwire.Message{asst("", call("c1", tool.IDSleep), call("c2", "read"))},
			want: []string{"c1"},
		},
		{
			name: "answered calls are not protected",
			msgs: []llmwire.Message{asst("", call("c1", tool.IDSleep)), toolRes("c1")},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := newTestAgent()
			agent.stagedCalls = map[string]string{}
			for _, msg := range tt.msgs {
				for _, c := range msg.ToolCalls {
					if tool.IsExternalCall(c.Name) {
						agent.stagedCalls[c.ID] = c.Name
					}
				}
			}
			setTestMessages(agent, tt.msgs)

			got := agent.pendingExternalCallIDs()

			assert.Len(t, got, len(tt.want))

			for _, id := range tt.want {
				assert.True(t, got[id], id)
			}
		})
	}
}

// Repair stubs dangling calls so the provider never sees an unanswered tool_use.
// A pending external call is the one thing it must leave alone: the verdict needs
// that tool_use still open to land on.
func TestRepair_LeavesAPendingExternalCallAlone(t *testing.T) {
	msgs := []llmwire.Message{
		usr("replace the config"),
		asst("", call("c1", tool.IDConfigEdit)),
		usr("hurry up"),
	}

	agent := newTestAgent()
	agent.stagedCalls = map[string]string{"c1": tool.IDConfigEdit}
	setTestMessages(agent, msgs)

	repaired := repairTranscriptExcluding(msgs, agent.pendingExternalCallIDs())

	for _, m := range repaired {
		assert.NotEqual(t, "c1", m.ToolCallID, "the open tool_use must not be stubbed")
	}

	// Without the exclusion it would be stubbed — which is what makes the
	// exclusion load-bearing rather than decorative.
	stubbed := repairTranscriptExcluding(msgs, nil)
	found := false

	for _, m := range stubbed {
		if m.ToolCallID == "c1" {
			found = true
		}
	}

	assert.True(t, found)
}

func TestImageBase64Bytes(t *testing.T) {
	assert.Equal(t, int64(0), imageBase64Bytes(0))
	assert.Equal(t, int64(4), imageBase64Bytes(1), "ceil(size/3)*4")
	assert.Equal(t, int64(4), imageBase64Bytes(3))
	assert.Equal(t, int64(8), imageBase64Bytes(4))
}

func TestShouldCompactImageBytePressure(t *testing.T) {
	const window = 1_048_576

	// The incident shape: 40 page scans, ~122 MB base64 at ~18% token
	// occupancy — the token projection sat far below the trigger, and the
	// count axis was over its mark too.
	incident := []llmwire.Message{{Role: llmwire.RoleSystem, Content: "sys"}}
	for i := range 40 {
		incident = append(incident, pageScan(fmt.Sprintf("p%d", i), 2_290_000))
	}

	agent := newTestAgent()
	setTestMessages(agent, incident)

	_, count := imagePressure(incident)
	assert.Greater(t, count, imageCountHighWater)
	assert.True(t, agent.shouldCompact(window), "the byte wall triggers compaction")

	// The byte axis fires on its own: 15 oversized scans breach 12 MB while
	// the count axis stays quiet.
	byteOnly := []llmwire.Message{{Role: llmwire.RoleSystem, Content: "sys"}}
	for i := range 15 {
		byteOnly = append(byteOnly, pageScan(fmt.Sprintf("b%d", i), 5_000_000))
	}

	setTestMessages(agent, byteOnly)

	bytes, count := imagePressure(byteOnly)
	assert.Greater(t, bytes, int64(imageBytesHighWater))
	assert.LessOrEqual(t, count, imageCountHighWater)

	size, _ := agent.projectContextSize()
	assert.Less(t, size, compactionCutoff(window), "the token axis alone stays quiet")
	assert.True(t, agent.shouldCompact(window))
}

func TestShouldCompactImageCountPressure(t *testing.T) {
	// 21 tiny crops pass any byte budget and still break the count limit (D5).
	transcript := []llmwire.Message{{Role: llmwire.RoleSystem, Content: "sys"}}
	for i := range 21 {
		transcript = append(transcript, pageScan(fmt.Sprintf("c%d", i), 3_000))
	}

	agent := newTestAgent()
	setTestMessages(agent, transcript)

	bytes, count := imagePressure(transcript)
	assert.Less(t, bytes, int64(imageBytesHighWater))
	assert.Greater(t, count, imageCountHighWater)
	assert.True(t, agent.shouldCompact(1_048_576))

	// Equality is not a breach: 20 crops stay quiet, on either axis.
	setTestMessages(agent, transcript[:21])
	_, count = imagePressure(transcript[:21])
	assert.Equal(t, imageCountHighWater, count)
	assert.False(t, agent.shouldCompact(1_048_576))
}

func TestShouldCompactFourScansStayQuiet(t *testing.T) {
	transcript := []llmwire.Message{{Role: llmwire.RoleSystem, Content: "sys"}}
	for i := range 4 {
		transcript = append(transcript, pageScan(fmt.Sprintf("p%d", i), 2_000_000))
	}

	agent := newTestAgent()
	setTestMessages(agent, transcript)

	bytes, _ := imagePressure(transcript)
	assert.Less(t, bytes, int64(imageBytesHighWater), "the fixture total is what matters, not its count")
	assert.False(t, agent.shouldCompact(1_048_576))
}

// The numbers come from the incident: three page scans of identical pixel
// dimensions cost 4,699 / 4,700 / 4,699 provider tokens regardless of a 1.24×
// file-size spread, which fixes the charge at ⌈1615/28⌉ × ⌈2193/28⌉ = 58 × 79.
func TestImageTokenEstimatePatchQuantum(t *testing.T) {
	ref := llmwire.ImageRef{
		Path: "/tmp/page.png", Mime: llmwire.MimeImagePng, Size: 3_200_000,
		Width: 1615, Height: 2193,
	}

	assert.Equal(t, 4582, imageTokenEstimate(ref))
}

func TestImageTokenEstimateFallbackWithoutDimensions(t *testing.T) {
	assert.Equal(t, 0, imageTokenEstimate(llmwire.ImageRef{Size: 0}), "Size=0 stays free")
	assert.Equal(t, 1, imageTokenEstimate(llmwire.ImageRef{Size: 5}), "small files charge Size/4")
	assert.Equal(t, 8192, imageTokenEstimate(llmwire.ImageRef{Size: 1 << 30}), "huge files cap at the ceiling")
}

func TestShouldCompact(t *testing.T) {
	const window = 80000

	cutoff := compactionCutoff(window) // 68000

	tests := []struct {
		name     string
		tokens   int
		baseline *contextBaseline
		want     bool
	}{
		{"below cutoff, unmeasured", cutoff - 1000, nil, false},
		{"above cutoff, unmeasured", cutoff + 1000, nil, true},
		{"at cutoff exactly is not over", cutoff, nil, false},
		{
			"measurement outranks the estimate downward",
			cutoff + 1000,
			&contextBaseline{promptTokens: 1000, messageCount: 1},
			false,
		},
		{
			"measurement outranks the estimate upward",
			cutoff - 1000,
			&contextBaseline{promptTokens: cutoff + 1, messageCount: 1},
			true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			agent := newTestAgent()
			agent.prompt = sessionprompt.NewBuilder("", "") // zero overhead: the cutoff cases are exact
			setTestMessages(agent, buildMessagesWithTokens(tc.tokens))
			if tc.baseline != nil {
				agent.storeContextBaseline(
					tc.baseline.promptTokens,
					tc.baseline.messageCount,
					agent.modelGeneration(),
				)
			}

			assert.Equal(t, tc.want, agent.shouldCompact(window))
		})
	}
}

// The delta is counted over the tail alone: the measured prefix keeps its
// measured cost no matter what len/4 thinks of it.
func TestProjectContextSizeCountsOnlyTheTailAfterTheBaseline(t *testing.T) {
	msgs := append(buildMessagesWithTokens(50000), buildMessagesWithTokens(1000)...)
	base := &contextBaseline{promptTokens: 9000, messageCount: 1}

	assert.Equal(t, 10000, projectContextSize(msgs, base, 777))
	assert.Equal(t, 51777, projectContextSize(msgs, nil, 777), "no measurement: whole transcript plus overhead")
}

// A baseline that no longer indexes the transcript (compaction shrank it under
// the recorded position) must not be trusted into an inflated projection.
func TestProjectContextSizeDiscardsAStaleBaseline(t *testing.T) {
	msgs := buildMessagesWithTokens(1000)
	base := &contextBaseline{promptTokens: 90000, messageCount: 40}

	assert.Equal(t, 1100, projectContextSize(msgs, base, 100))
}

// TestEstimateTokensCountsToolCallArguments pins that write/edit/apply_patch file
// bodies (carried in tool-call Arguments) are part of the trigger estimate.
func TestEstimateTokensCountsToolCallArguments(t *testing.T) {
	args := []byte(strings.Repeat("x", 4000)) // ~1000 est tokens
	msgs := []llmwire.Message{{
		Role:      llmwire.RoleAssistant,
		ToolCalls: []llmwire.ToolCall{{ID: "c1", Name: "write", Arguments: args}},
	}}

	assert.Equal(t, 1000, estimateTokens(msgs))
}

func TestStoredMessageCarriesReasoningRaw(t *testing.T) {
	stored, err := storedMessage(&llmwire.Message{
		Role:         llmwire.RoleAssistant,
		Content:      "hi",
		ReasoningRaw: reasoningBlob,
	})
	require.NoError(t, err)
	assert.JSONEq(t, string(reasoningBlob), string(stored.ReasoningRaw))
}

func TestReloadMessagesRestoresReasoningRaw(t *testing.T) {
	ms := newMessageStore(&reasoningLoadStore{
		messages: []*transcript.Message{
			{ID: 1, Role: llmwire.RoleUser, Content: "go"},
			{ID: 2, Role: llmwire.RoleAssistant, Content: "sure", ReasoningRaw: reasoningBlob},
		},
	}, 1)

	require.NoError(t, ms.reloadMessages(context.Background()))

	msgs := ms.getMessages()
	require.Len(t, msgs, 2)
	assert.Nil(t, msgs[0].ReasoningRaw)
	assert.JSONEq(t, string(reasoningBlob), string(msgs[1].ReasoningRaw))
}

func TestRepairTranscript_NoOrphans(t *testing.T) {
	msgs := []llmwire.Message{
		{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "c1", Name: "read"}}},
		{Role: llmwire.RoleTool, ToolCallID: "c1", ToolName: "read", Content: "ok"},
	}
	result := repairTranscript(msgs)
	assert.Len(t, result, 2)
}

func TestRepairTranscript_RemovesOrphans(t *testing.T) {
	msgs := []llmwire.Message{
		{Role: llmwire.RoleTool, ToolCallID: "orphan1", ToolName: "grep", Content: "stale"},
		{Role: llmwire.RoleTool, ToolCallID: "orphan2", ToolName: "read", Content: "stale"},
		{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "c1", Name: "bash"}}},
		{Role: llmwire.RoleTool, ToolCallID: "c1", ToolName: "bash", Content: "ok"},
	}
	result := repairTranscript(msgs)
	assert.Len(t, result, 2)
	assert.Equal(t, llmwire.RoleAssistant, result[0].Role)
	assert.Equal(t, "c1", result[1].ToolCallID)
}

func TestRepairTranscriptExcluding_DoesNotStubPendingCall(t *testing.T) {
	// A genuinely-pending external call (e.g. a suspended blocking task) must NOT
	// get a synthetic result — stubbing it would corrupt the resume.
	msgs := []llmwire.Message{
		{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{
			{ID: "task-1", Name: "task"},
			{ID: "read-1", Name: "read"},
		}},
		{Role: llmwire.RoleTool, ToolCallID: "read-1", ToolName: "read", Content: "ok"},
	}

	result := repairTranscriptExcluding(msgs, map[string]bool{"task-1": true})

	// read-1 keeps its result; task-1 is left unmatched (no synthetic stub).
	for _, m := range result {
		if m.Role == llmwire.RoleTool && m.ToolCallID == "task-1" {
			t.Fatalf("pending task-1 must not be stubbed, got %+v", m)
		}
	}

	assert.Len(t, result, 2, "assistant turn + read result only; pending task untouched")
}

func TestRepairTranscriptExcluding_StubsNonExcludedMissingResult(t *testing.T) {
	// Without exclusion, a missing result IS stubbed — the exclude set is the only
	// thing that suppresses it.
	msgs := []llmwire.Message{
		{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "task-1", Name: "task"}}},
	}

	result := repairTranscriptExcluding(msgs, nil)

	assert.Len(t, result, 2)
	assert.Equal(t, "task-1", result[1].ToolCallID)
	assert.Contains(t, result[1].Content, "transcript repair")
}

func TestRepairTranscript_SyntheticErrorForMissingResult(t *testing.T) {
	msgs := []llmwire.Message{
		{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{
			{ID: "c1", Name: "read"},
			{ID: "c2", Name: "grep"},
		}},
		{Role: llmwire.RoleTool, ToolCallID: "c1", ToolName: "read", Content: "file content"},
	}
	result := repairTranscript(msgs)
	assert.Len(t, result, 3)
	assert.Equal(t, "c1", result[1].ToolCallID)
	assert.Equal(t, "c2", result[2].ToolCallID)
	assert.Contains(t, result[2].Content, "transcript repair")
}

func TestRepairTranscript_ReordersResults(t *testing.T) {
	msgs := []llmwire.Message{
		{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{
			{ID: "c1", Name: "read"},
			{ID: "c2", Name: "grep"},
		}},
		{Role: llmwire.RoleTool, ToolCallID: "c2", ToolName: "grep", Content: "found"},
		{Role: llmwire.RoleTool, ToolCallID: "c1", ToolName: "read", Content: "content"},
	}
	result := repairTranscript(msgs)
	assert.Len(t, result, 3)
	assert.Equal(t, "c1", result[1].ToolCallID)
	assert.Equal(t, "c2", result[2].ToolCallID)
}

func TestRepairTranscript_SkipsSyntheticForIncompleteToolCalls(t *testing.T) {
	msgs := []llmwire.Message{
		{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{
			{ID: "c1", Name: "read"},
			{ID: "c2", Name: ""},
		}},
		{Role: llmwire.RoleTool, ToolCallID: "c1", ToolName: "read", Content: "ok"},
	}
	result := repairTranscript(msgs)
	assert.Len(t, result, 2)
	assert.Equal(t, "c1", result[1].ToolCallID)
}

func TestRepairTranscript_Empty(t *testing.T) {
	result := repairTranscript(nil)
	assert.Nil(t, result)
}

// Durable call names determine which owner must settle each unanswered call.
func TestUnresolvedStoredExternalCalls(t *testing.T) {
	tests := []struct {
		name string
		msgs []*transcript.Message
		want []PendingToolCall
	}{
		{
			name: "an unresolved external call is pending",
			msgs: []*transcript.Message{storedAssistant(`[{"id":"c1","name":"config_edit"}]`)},
			want: []PendingToolCall{{ID: "c1", Name: tool.IDConfigEdit}},
		},
		{
			name: "an answered call is not",
			msgs: []*transcript.Message{
				storedAssistant(`[{"id":"c1","name":"config_edit"}]`),
				storedToolResult("c1"),
			},
		},
		{
			name: "an unresolved in-loop tool is pending",
			msgs: []*transcript.Message{storedAssistant(`[{"id":"c1","name":"bash"}]`)},
			want: []PendingToolCall{{ID: "c1", Name: "bash"}},
		},
		{
			name: "a call with no id cannot be answered and is skipped",
			msgs: []*transcript.Message{storedAssistant(`[{"name":"sleep"}]`)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := UnresolvedStoredCalls(tt.msgs)
			require.NoError(t, err)
			if len(tt.want) == 0 {
				assert.Empty(t, got)
			} else {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

// Corrupt tool-call rows must fail the sweep rather than hide pending work.
func TestUnresolvedStoredExternalCalls_UndecodableRow(t *testing.T) {
	_, err := UnresolvedStoredCalls([]*transcript.Message{storedAssistant(`{`)})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode tool calls")
}

func storedAssistant(toolCalls string) *transcript.Message {
	return &transcript.Message{Role: "assistant", ToolCalls: []byte(toolCalls)}
}

func storedToolResult(callID string) *transcript.Message {
	return &transcript.Message{Role: "tool", ToolCallID: callID, Content: "done"}
}
