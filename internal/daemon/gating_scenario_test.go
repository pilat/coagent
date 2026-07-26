package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/tool"
)

// A subagent must never reach the control plane, whatever its allowlist says.
var controlPlaneTools = []string{tool.IDSleep, tool.IDTask, tool.IDSchedule}

var configPlaneTools = []string{tool.IDSetProvider, tool.IDSetDefaultModel}

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

// A built-in explore subagent is read-only by allowlist. The daemon registers
// task/sleep/schedule onto every live session after construction, so only
// RegisterGatedTool's re-check keeps them out of the child.
func TestIntegration_ExploreChildIsDeniedControlPlaneTools(t *testing.T) {
	const exploreCallID = "task-explore-1"

	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "CHILD_EXPLORE") {
			return probeMissingTools(msgs, "explore", controlPlaneTools)
		}

		if hasToolResultFor(msgs, tool.IDTask) || hasToolResultFor(msgs, "subagent_event") {
			return &llmwire.Response{Text: "parent done"}
		}

		return &llmwire.Response{
			ToolCalls: []llmwire.ToolCall{spawnTaskCall(exploreCallID, "explore", "CHILD_EXPLORE")},
		}
	}

	h := newGatingHarness(t, nil, respond)
	defer h.shutdown()

	parentID, err := h.mgr.Send(h.ctx, h.projectID, "spawn an explore child", "fake-model", nil)
	require.NoError(t, err)

	link := h.waitForLink(parentID, exploreCallID)
	h.waitForDelivery(link.ChildID)
	h.mgr.waitIdle(parentID)

	h.assertUnknownTools(link.ChildID, controlPlaneTools)

	offered := h.schemas.offered(link.ChildID)
	assert.Contains(t, offered, "read", "explore keeps the tools its own allowlist grants")
	assertNotOffered(t, offered, append(append([]string{}, controlPlaneTools...), configPlaneTools...))

	// Without this the child assertions would pass on a daemon that registers
	// nothing at all.
	parentOffered := h.schemas.offered(parentID)
	for _, id := range append(append([]string{}, controlPlaneTools...), configPlaneTools...) {
		assert.Contains(t, parentOffered, id, "root session must keep %q", id)
	}

	require.NoError(t, llm.ValidateToolPairing(h.parentMessages(parentID)))
}

// Project-defined subagents are outside the built-in taxonomy: a restricted one
// is held to its own list, and even an unrestricted ("*") one stays off the
// config plane, which is guarded by parentage rather than by the allowlist.
func TestIntegration_ProjectSubagentToolGating(t *testing.T) {
	const (
		scoutCallID = "task-scout-1"
		wideCallID  = "task-wide-1"
	)

	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "CHILD_SCOUT") {
			return probeMissingTools(msgs, "scout", controlPlaneTools)
		}

		if hasUserContaining(msgs, "CHILD_WIDE") {
			return probeMissingTools(msgs, "wide", configPlaneTools)
		}

		if hasToolResultFor(msgs, tool.IDTask) || hasToolResultFor(msgs, "subagent_event") {
			return &llmwire.Response{Text: "parent done"}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{
			spawnTaskCall(scoutCallID, "scout", "CHILD_SCOUT"),
			spawnTaskCall(wideCallID, "wide", "CHILD_WIDE"),
		}}
	}

	agents := map[string]string{"scout.md": scoutAgentFile, "wide.md": wideAgentFile}

	h := newGatingHarness(t, agents, respond)
	defer h.shutdown()

	parentID, err := h.mgr.Send(h.ctx, h.projectID, "spawn project children", "fake-model", nil)
	require.NoError(t, err)

	scout := h.waitForLink(parentID, scoutCallID)
	wide := h.waitForLink(parentID, wideCallID)
	h.waitForDelivery(scout.ChildID)
	h.waitForDelivery(wide.ChildID)
	h.mgr.waitIdle(parentID)

	h.assertUnknownTools(scout.ChildID, controlPlaneTools)
	scoutOffered := h.schemas.offered(scout.ChildID)
	assert.Contains(t, scoutOffered, "read")
	assert.Contains(t, scoutOffered, "grep")
	assertNotOffered(t, scoutOffered, append([]string{"ls", "bash"}, controlPlaneTools...))
	assertNotOffered(t, scoutOffered, configPlaneTools)

	h.assertUnknownTools(wide.ChildID, configPlaneTools)
	wideOffered := h.schemas.offered(wide.ChildID)
	for _, id := range controlPlaneTools {
		assert.Contains(
			t,
			wideOffered,
			id,
			`a "*" subagent does gain %q — so its config-plane gap is the parentage gate`,
			id,
		)
	}

	assertNotOffered(t, wideOffered, configPlaneTools)
}
