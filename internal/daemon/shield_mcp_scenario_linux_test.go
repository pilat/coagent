//go:build linux

package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/mcpstore"
)

func TestShieldRaiseRetiresIdleMCPAndRaisedActivationRediscovers(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap is not installed")
	}
	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		last := lastUserText(messages)
		if strings.Contains(last, "raised activation") {
			if hasToolResultForCallID(messages, "raised-ping") {
				return &llmwire.Response{Text: "raised complete"}
			}
			return mcpPingCall("raised-ping")
		}
		if hasToolResultForCallID(messages, "down-ping") {
			return &llmwire.Response{Text: "down complete"}
		}

		return mcpPingCall("down-ping")
	}
	h, registry, _ := newMCPHarnessConfigured(t, respond, 0, func(cfg *config.Config) {
		cfg.UnifiedConfig = &config.UnifiedConfig{}
		cfg.UnifiedConfig.Sandbox.Enabled = true
	})
	defer h.shutdown()
	h.mgr.sandboxEnabled = true
	workDir, err := h.mgr.store.GetProjectWorkDir(h.ctx, h.projectID)
	require.NoError(t, err)
	fake := &fakeMCPServer{
		path: filepath.Join(workDir, "fake-mcp.sh"), log: filepath.Join(workDir, "mcp-events.log"),
		pong: "pong",
	}
	require.NoError(t, os.WriteFile(fake.path, []byte(fakeMCPScript), 0o700))
	require.NoError(t, os.WriteFile(fake.log, nil, 0o600))
	require.NoError(t, registry.Add(h.ctx, &h.projectID, mcpstore.ServerDef{
		Name: "fake", Command: fake.path, Args: []string{fake.log, fake.pong, ""}, Enabled: true,
	}))
	root, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", map[string]any{
		controllerapi.SessionAttributeManagerID: scenarioManagerID,
	})
	require.NoError(t, err)
	controller := newChainController(t, h)

	require.NoError(t, h.mgr.SendToSession(h.ctx, root.ID, "use the down-policy server"))
	h.waitUntil("down MCP call completes", func() bool {
		return lastAssistantTextDTO(h.parentMessages(root.ID)) == "down complete"
	})
	h.mgr.waitIdle(root.ID)
	assert.Equal(t, 1, fake.count(t, "spawn"))
	ackUntilOutputContent(t, controller, "down complete")

	require.NoError(t, h.mgr.SendToSession(h.ctx, root.ID, shieldsUpCommand))
	for index := range 2 {
		output, claimErr := controller.ClaimOutput(h.ctx)
		require.NoError(t, claimErr)
		require.NoError(t, controller.AckOutput(h.ctx, controllerapi.OutputAckData{
			ID: output.ID, AttemptID: output.AttemptID,
			MessageIDs: []string{strings.Repeat("s", index+1)},
		}))
	}
	require.NoError(t, h.mgr.SendToSession(h.ctx, root.ID, "use the raised activation"))
	h.waitUntil("raised MCP call completes", func() bool {
		return lastAssistantTextDTO(h.parentMessages(root.ID)) == "raised complete"
	})
	assert.Equal(t, 2, fake.count(t, "spawn"))
	assert.Contains(t, toolResultForCallID(h.parentMessages(root.ID), "raised-ping"), "pong")
}

func ackUntilOutputContent(
	t *testing.T,
	controller controllerapi.OutputQueueController,
	want string,
) {
	t.Helper()
	for index := range 10 {
		output, err := controller.ClaimOutput(t.Context())
		require.NoError(t, err)
		require.NoError(t, controller.AckOutput(t.Context(), controllerapi.OutputAckData{
			ID: output.ID, AttemptID: output.AttemptID,
			MessageIDs: []string{"pre-shield-" + strings.Repeat("x", index+1)},
		}))
		if strings.Contains(output.Content, want) {
			return
		}
	}
	t.Fatalf("output containing %q was not delivered", want)
}
