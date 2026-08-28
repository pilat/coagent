package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/tool"
)

const (
	setManagerPatchCallID   = "set-manager-patch-1"
	setManagerReapplyCallID = "set-manager-reapply-1"
)

//nolint:gosec // test fixture with obviously fake credentials
const patchConfigSecrets = `WORK_API_KEY=sk-ant-work-0000000000
WHISPER_API_KEY=sk-openai-whisper-0000000000
MANAGER_BEFORE_BOT_TOKEN=1111:before-token
MANAGER_TG_BOT_TOKEN=1234:tg-token
MANAGER_AFTER_BOT_TOKEN=2222:after-token
`

// patchConfigYAML is a config with three managers; the middle one carries
// non-default values for every editable field, including Whisper.
const patchConfigYAML = `providers:
    work:
        driver: anthropic
        api_key: ${WORK_API_KEY}
    whisper:
        driver: openai
        api_key: ${WHISPER_API_KEY}
models:
    - id: claude-sonnet-5
      provider: work
managers:
    - id: before
      driver: telegram
      enabled: true
      bot_token: ${MANAGER_BEFORE_BOT_TOKEN}
      allowed_user_ids:
        - 1
      target_chat_id: -1
      service_topic_name: BeforeTopic
      service_topic_icon_emoji_id: "111"
      session_topic_icon_emoji_id: "222"
      send_chunk_delay_ms: 50
      poll_timeout_sec: 20
      whisper:
        provider: whisper
        model: whisper-1
    - id: tg
      driver: telegram
      enabled: true
      bot_token: ${MANAGER_TG_BOT_TOKEN}
      allowed_user_ids:
        - 7
      target_chat_id: -100
      service_topic_name: Support
      service_topic_icon_emoji_id: "123"
      session_topic_icon_emoji_id: "456"
      send_chunk_delay_ms: 200
      poll_timeout_sec: 45
      whisper:
        provider: whisper
        model: whisper-1
    - id: after
      driver: telegram
      enabled: true
      bot_token: ${MANAGER_AFTER_BOT_TOKEN}
      allowed_user_ids:
        - 2
      target_chat_id: -2
      service_topic_name: AfterTopic
      service_topic_icon_emoji_id: "333"
      session_topic_icon_emoji_id: "444"
      send_chunk_delay_ms: 150
      poll_timeout_sec: 60
`

func setManagerPatchRespond(_ string, msgs []llmwire.Message) *llmwire.Response {
	if hasToolResultFor(msgs, tool.IDSetManager) {
		return &llmwire.Response{Text: "manager settings updated"}
	}

	return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
		ID:        setManagerPatchCallID,
		Name:      tool.IDSetManager,
		Arguments: []byte(`{"id":"tg","allowed_user_ids":[7,9]}`),
	}}}
}

func setManagerReapplyRespond(_ string, msgs []llmwire.Message) *llmwire.Response {
	if hasToolResultFor(msgs, tool.IDSetManager) {
		return &llmwire.Response{Text: "manager reapplied"}
	}

	return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
		ID:        setManagerReapplyCallID,
		Name:      tool.IDSetManager,
		Arguments: []byte(`{"id":"tg","driver":"telegram"}`),
	}}}
}

// TestHarnessScenario_SetManagerPatchPreservesFields covers an allow-list-only update
// through suspend, commit, restart, and verdict delivery.
func TestHarnessScenario_SetManagerPatchPreservesFields(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apply.db")
	configDir := newApplyConfigDirWithSecrets(t, patchConfigYAML, patchConfigSecrets)
	before, err := config.LoadRawUnifiedConfig(filepath.Join(configDir, "config.yaml"))
	require.NoError(t, err)
	expectedManagers := append([]config.ManagerEntry(nil), before.Managers...)
	expectedManagers[1].AllowedUserIDs = []int64{7, 9}

	first := newApplyDaemonWith(t, dbPath, configDir, setManagerPatchRespond)
	collector1 := collectEvents(first.mgr.PubSub().SubscribeAll())

	sessionID, err := first.mgr.Send(
		first.ctx, first.projectID, "update manager allow list", "fake-model",
		map[string]any{"channel": "cli", "manager_id": "cli"},
	)
	require.NoError(t, err)

	first.waitForRestart(t)
	first.waitUntil("session suspended on the config call", func() bool {
		return !first.mgr.HasActiveLoop(sessionID)
	})

	// The staged change committed and left a marker.
	pending, err := first.ops.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, pending)
	require.Equal(t, sessionID, pending.SessionID)
	require.Equal(t, setManagerPatchCallID, pending.ToolCallID)
	require.Equal(t, tool.IDSetManager, pending.ToolName)

	msgs := first.parentMessages(sessionID)
	require.Equal(t, 1, countAssistantToolCallsFor(msgs, tool.IDSetManager))
	require.Zero(t, countToolResultsFor(msgs, tool.IDSetManager), "the call is out with the world")

	first.shutdown()
	collector1.stop()

	second := newApplyDaemonWith(t, dbPath, configDir, setManagerPatchRespond)
	defer second.shutdown()

	collector2 := collectEvents(second.mgr.PubSub().SubscribeAll())
	defer collector2.stop()

	require.NoError(t, second.mgr.Start(second.ctx))

	outcome, err := second.bootVerdict(t)
	require.NoError(t, err)
	require.True(t, outcome.Verdict.Applied, outcome.Verdict.Reason())

	assert.Equal(t, `update manager "tg" (allowed_user_ids)`, outcome.Pending.Summary)

	second.mgr.waitIdle(sessionID)

	// Transcript: one assistant tool call, one result, no re-execution.
	finalMsgs := second.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(finalMsgs))
	assert.Equal(t, 1, countAssistantToolCallsFor(finalMsgs, tool.IDSetManager))
	assert.Equal(t, 1, countToolResultsFor(finalMsgs, tool.IDSetManager))
	assert.Contains(t, lastToolResultContent(finalMsgs, tool.IDSetManager), "Config applied")
	assert.Equal(t, "manager settings updated", lastAssistantTextDTO(finalMsgs))

	// Raw config: sentinel managers untouched; tg preserves every field except
	// the allow-list.
	raw, err := config.LoadRawUnifiedConfig(filepath.Join(configDir, "config.yaml"))
	require.NoError(t, err)
	require.Len(t, raw.Managers, 3)

	assert.Equal(t, expectedManagers, raw.Managers)

	// Resolved config also agrees after defaults are applied.
	secrets, err := config.LoadSecretsFrom(filepath.Join(configDir, "secrets"))
	require.NoError(t, err)
	resolved, err := config.LoadUnifiedConfig(filepath.Join(configDir, "config.yaml"), secrets)
	require.NoError(t, err)
	assert.Equal(t, config.ManagerEntry{
		ID: "tg", Driver: "telegram", Enabled: boolPtr(true), BotToken: "1234:tg-token",
		AllowedUserIDs: []int64{7, 9}, TargetChatID: int64Ptr(-100), ServiceTopicName: "Support",
		ServiceTopicIconEmojiID: "123", SessionTopicIconEmojiID: "456", SendChunkDelayMS: 200,
		PollTimeoutSec: 45, Whisper: &config.ManagerWhisperEntry{Provider: "whisper", Model: "whisper-1"},
	}, resolved.Managers[1])

	// Marker is consumed.
	assert.NoFileExists(t, filepath.Join(configDir, "pending_apply.json"))

	collector2.waitFor(t, "resumed completion trace", func(events []controllerapi.SessionNotification) bool {
		return len(events) == 3
	})
	combined := append(collector1.snapshot(), collector2.snapshot()...)
	assertHarnessTrace(t, "set_manager_patch_restart.json", combined, sessionID)
}

// TestHarnessScenario_SetManagerReapplyRestarts verifies a no-op patch still restarts.
func TestHarnessScenario_SetManagerReapplyRestarts(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apply.db")
	configDir := newApplyConfigDirWithSecrets(t, patchConfigYAML, patchConfigSecrets)
	before, err := config.LoadRawUnifiedConfig(filepath.Join(configDir, "config.yaml"))
	require.NoError(t, err)

	first := newApplyDaemonWith(t, dbPath, configDir, setManagerReapplyRespond)
	collector1 := collectEvents(first.mgr.PubSub().SubscribeAll())

	sessionID, err := first.mgr.Send(
		first.ctx, first.projectID, "reapply manager", "fake-model",
		map[string]any{"channel": "cli", "manager_id": "cli"},
	)
	require.NoError(t, err)

	first.waitForRestart(t)
	first.waitUntil("session suspended on the config call", func() bool {
		return !first.mgr.HasActiveLoop(sessionID)
	})

	first.shutdown()
	collector1.stop()

	second := newApplyDaemonWith(t, dbPath, configDir, setManagerReapplyRespond)
	defer second.shutdown()

	collector2 := collectEvents(second.mgr.PubSub().SubscribeAll())
	defer collector2.stop()

	require.NoError(t, second.mgr.Start(second.ctx))

	outcome, err := second.bootVerdict(t)
	require.NoError(t, err)
	require.True(t, outcome.Verdict.Applied, outcome.Verdict.Reason())

	assert.Equal(t, `reapply manager "tg" (no config changes)`, outcome.Pending.Summary)

	second.mgr.waitIdle(sessionID)

	finalMsgs := second.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(finalMsgs))
	assert.Equal(t, 1, countAssistantToolCallsFor(finalMsgs, tool.IDSetManager))
	assert.Equal(t, 1, countToolResultsFor(finalMsgs, tool.IDSetManager))
	assert.Contains(t, lastToolResultContent(finalMsgs, tool.IDSetManager), "Config applied")
	assert.Equal(t, "manager reapplied", lastAssistantTextDTO(finalMsgs))

	// Parsed raw entry is semantically unchanged.
	raw, err := config.LoadRawUnifiedConfig(filepath.Join(configDir, "config.yaml"))
	require.NoError(t, err)
	assert.Equal(t, before.Managers, raw.Managers)

	collector2.waitFor(t, "resumed completion trace", func(events []controllerapi.SessionNotification) bool {
		return len(events) == 3
	})
	combined := append(collector1.snapshot(), collector2.snapshot()...)
	assertHarnessTrace(t, "set_manager_reapply_restart.json", combined, sessionID)
}

func newApplyConfigDirWithSecrets(t *testing.T, configYAML, secrets string) string {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(configYAML), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "secrets"), []byte(secrets), 0o600))

	return dir
}

func boolPtr(value bool) *bool { return &value }

func int64Ptr(value int64) *int64 { return &value }
