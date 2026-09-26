//go:build linux

package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llmwire"
)

func TestShieldedCompiledSessionBlocksReportedHostAudit(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap is not installed")
	}
	base := t.TempDir()
	home := filepath.Join(base, "home")
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".ssh"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".ssh", "id_rsa.pub"), []byte("host key"), 0o600))
	t.Setenv("HOME", home)

	result := make(chan string, 1)
	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasToolResultFor(messages, "bash") {
			for _, message := range slices.Backward(messages) {
				if message.Role == llmwire.RoleTool && message.ToolName == "bash" {
					result <- message.Content
					break
				}
			}
			return &llmwire.Response{Text: "audit complete"}
		}
		command := strings.Join([]string{
			"test ! -e ~/.ssh/id_rsa.pub",
			"test -w /tmp",
			"test -r /etc/hosts",
			"test ! -e /dev/sda",
			"test $(find /proc -maxdepth 1 -type d -name '[0-9]*' | wc -l) -le 8",
			"printf SHIELDED_AUDIT_OK",
		}, " && ")
		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID: "audit-bash", Name: "bash", Arguments: []byte(fmt.Sprintf(`{"command":%q}`, command)),
		}}}
	}
	h := newSubagentHarnessOnDBWithProjectConfig(
		t, filepath.Join(t.TempDir(), "shielded.db"), respond, nil,
		func(cfg *config.Config) {
			cfg.UnifiedConfig = &config.UnifiedConfig{}
			cfg.UnifiedConfig.Sandbox.Enabled = true
		},
	)
	defer h.shutdown()
	h.mgr.sandboxEnabled = true
	root, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", map[string]any{
		controllerapi.SessionAttributeManagerID: scenarioManagerID,
	})
	require.NoError(t, err)
	controller := newChainController(t, h)

	require.NoError(t, h.mgr.SendToSession(h.ctx, root.ID, shieldsUpCommand))
	for index := range 2 {
		output, claimErr := controller.ClaimOutput(h.ctx)
		require.NoError(t, claimErr)
		require.NoError(t, controller.AckOutput(h.ctx, controllerapi.OutputAckData{
			ID: output.ID, AttemptID: output.AttemptID,
			MessageIDs: []string{fmt.Sprintf("shield-%d", index)},
		}))
	}
	require.NoError(t, h.mgr.SendToSession(h.ctx, root.ID, "run the sanitized access audit"))

	select {
	case output := <-result:
		assert.Contains(t, output, "SHIELDED_AUDIT_OK")
	case <-time.After(10 * time.Second):
		t.Fatal("compiled shielded session did not finish the Bash audit")
	}
	record, err := h.sessStore.GetSession(h.ctx, root.ID)
	require.NoError(t, err)
	assert.True(t, record.ShieldsUp)
}
