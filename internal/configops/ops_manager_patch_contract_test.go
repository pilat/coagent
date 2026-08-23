package configops

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
)

func TestSetManagerPatch_ResetsRawValuesToResolvedDefaults(t *testing.T) {
	f := newFixture(t, managerConfigWithOptionalFields, baseSecrets)
	empty := ""
	zero := 0

	raw := f.applied(t, SetManagerPatch(ManagerPatch{
		ID: "tg", ServiceTopicName: &empty, ServiceTopicIconEmojiID: &empty,
		SessionTopicIconEmojiID: &empty, SendChunkDelayMS: &zero, PollTimeoutSec: &zero,
	}))
	assert.Equal(t, config.ManagerEntry{
		ID: "tg", Driver: "telegram", Enabled: boolptr(true), BotToken: Ref("MANAGER_TG_BOT_TOKEN"),
		AllowedUserIDs: []int64{7}, TargetChatID: int64ptr(-100),
	}, raw.Managers[0])

	secrets, err := config.LoadSecretsFrom(f.secretPath)
	require.NoError(t, err)
	resolved, err := config.LoadUnifiedConfig(filepath.Join(filepath.Dir(f.configPath), "config.yaml"), secrets)
	require.NoError(t, err)
	assert.Equal(t, "Coagent", resolved.Managers[0].ServiceTopicName)
	assert.Equal(t, 100, resolved.Managers[0].SendChunkDelayMS)
	assert.Equal(t, 30, resolved.Managers[0].PollTimeoutSec)
}

func TestSetManagerPatch_CreateCanBeDisabledAndRotateReference(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)
	newRef := Ref("NEW_MANAGER_TOKEN")
	_, verdict := f.svc.SetSecret("NEW_MANAGER_TOKEN", "5555:replacement-token")
	require.True(t, verdict.Applied)

	cfg := f.applied(t, SetManagerPatch(ManagerPatch{
		ID: "tg2", Driver: strptr("telegram"), Enabled: boolptr(false), BotToken: &newRef,
		AllowedUserIDs: &[]int64{8}, TargetChatID: int64ptr(-101),
	}))
	require.NotNil(t, cfg.Managers[1].Enabled)
	assert.False(t, *cfg.Managers[1].Enabled)

	cfg = f.applied(t, SetManagerPatch(ManagerPatch{ID: "tg", BotToken: &newRef}))
	assert.Equal(t, newRef, cfg.Managers[0].BotToken)
}

func TestSetManagerPatch_TargetAndSummaryContract(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)

	staged, verdict := f.svc.Stage(SetManagerPatch(ManagerPatch{ID: "tg", TargetChatID: int64ptr(-100)}))
	require.True(t, verdict.Applied)
	assert.Equal(t, `reapply manager "tg" (no config changes)`, staged.Summary)

	zero := int64(0)
	verdict = f.rejected(t, SetManagerPatch(ManagerPatch{ID: "tg", TargetChatID: &zero}))
	assert.Contains(t, verdict.Reason(), "forum target cannot change")

	_, verdict = f.svc.SetSecret("CONTROL_TOKEN", "6666:control-token")
	require.True(t, verdict.Applied)
	staged, verdict = f.svc.Stage(SetManagerPatch(ManagerPatch{
		ID: "tg\nname", Driver: strptr("telegram"), BotToken: strptr(Ref("CONTROL_TOKEN")),
		AllowedUserIDs: &[]int64{9}, TargetChatID: int64ptr(-102),
	}))
	require.True(t, verdict.Applied)
	assert.Equal(t, `add manager "tg\nname"`, staged.Summary)
	assert.NotContains(t, staged.Summary, "\n")
}

const managerConfigWithOptionalFields = `providers:
    work:
        driver: anthropic
        api_key: ${WORK_API_KEY}
managers:
    - id: tg
      driver: telegram
      enabled: true
      bot_token: ${MANAGER_TG_BOT_TOKEN}
      allowed_user_ids: [7]
      target_chat_id: -100
      service_topic_name: Support
      service_topic_icon_emoji_id: "123"
      session_topic_icon_emoji_id: "456"
      send_chunk_delay_ms: 200
      poll_timeout_sec: 45
`
