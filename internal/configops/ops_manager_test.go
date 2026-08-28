package configops

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
)

func TestSetManagerPatch_CreateSummary(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)

	_, v := f.svc.SetSecret("MANAGER_TG2_BOT_TOKEN", "9999:another-token-value")
	require.True(t, v.Applied)

	staged, v := f.svc.Stage(SetManagerPatch(ManagerPatch{
		ID: "tg2", Driver: strptr("telegram"), BotToken: strptr(Ref("MANAGER_TG2_BOT_TOKEN")),
		AllowedUserIDs: &[]int64{7}, TargetChatID: int64ptr(-100),
	}))
	require.True(t, v.Applied)
	assert.Equal(t, `add manager "tg2"`, staged.Summary)
}

func TestSetManagerPatch_UpdateSummaryListsChangedFields(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)

	staged, v := f.svc.Stage(SetManagerPatch(ManagerPatch{
		ID: "tg", AllowedUserIDs: &[]int64{7, 9},
	}))
	require.True(t, v.Applied)
	assert.Equal(t, `update manager "tg" (allowed_user_ids)`, staged.Summary)
}

func TestSetManagerPatch_NoOpSummary(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)

	staged, v := f.svc.Stage(SetManagerPatch(ManagerPatch{
		ID: "tg", Driver: strptr("telegram"),
	}))
	require.True(t, v.Applied)
	assert.Equal(t, `reapply manager "tg" (no config changes)`, staged.Summary)
}

func TestSetManagerPatch_ReservedCliID(t *testing.T) {
	assert.Equal(t, controllerapi.BuiltinCLIManagerID, reservedCLIManagerID)
	f := newFixture(t, baseConfig, baseSecrets)

	v := f.rejected(t, SetManagerPatch(ManagerPatch{
		ID: "cli", Driver: strptr("telegram"), BotToken: strptr(Ref("TOK")),
		AllowedUserIDs: &[]int64{7},
	}))
	assert.Contains(t, v.Reason(), `reserved`)
}

func TestSetManagerPatch_ExplicitEmptyTokenPreservesReference(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)
	empty := ""

	cfg := f.applied(t, SetManagerPatch(ManagerPatch{ID: "tg", BotToken: &empty}))
	assert.Equal(t, "${MANAGER_TG_BOT_TOKEN}", cfg.Managers[0].BotToken)
}

func TestManagerSlicesEqualDistinguishesNilAndEmpty(t *testing.T) {
	assert.False(t, managerSlicesEqual([]int64(nil), []int64{}))
	assert.True(t, managerSlicesEqual([]int64{}, []int64{}))
}

func TestSetManagerPatch_PreservesOmittedFieldsOnUpdate(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)

	cfg := f.applied(t, SetManagerPatch(ManagerPatch{
		ID: "tg", AllowedUserIDs: &[]int64{7, 9},
	}))

	require.Len(t, cfg.Managers, 1)
	m := cfg.Managers[0]
	assert.Equal(t, "telegram", m.Driver)
	assert.Equal(t, "${MANAGER_TG_BOT_TOKEN}", m.BotToken)
	require.NotNil(t, m.TargetChatID)
	assert.Equal(t, int64(-100), *m.TargetChatID)
	assert.Equal(t, []int64{7, 9}, m.AllowedUserIDs)
}

func TestSetManagerPatch_ReplaceWhisper(t *testing.T) {
	configWithWhisper := `providers:
    work:
        driver: anthropic
        api_key: ${WORK_API_KEY}
    router:
        driver: openrouter
        api_key: ${ROUTER_API_KEY}
        base_url: https://openrouter.ai/api/v1
models:
    - id: claude-sonnet-5
      provider: work
    - id: anthropic/claude-sonnet-5
      provider: router
managers:
    - id: tg
      driver: telegram
      enabled: true
      bot_token: ${MANAGER_TG_BOT_TOKEN}
      allowed_user_ids:
        - 7
      target_chat_id: -100
      whisper:
        provider: openai
        model: whisper-1
`
	f := newFixture(t, configWithWhisper, baseSecrets)

	cfg := f.applied(t, SetManagerPatch(ManagerPatch{
		ID: "tg", Whisper: WhisperPatch{Set: true, Value: nil},
	}))
	assert.Nil(t, cfg.Managers[0].Whisper)
}

func TestSetManagerPatch_DriverCannotChange(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)

	v := f.rejected(t, SetManagerPatch(ManagerPatch{
		ID: "tg", Driver: strptr("slack"),
	}))
	assert.Contains(t, v.Reason(), "driver cannot change")
}

func TestSetManagerPatch_TargetChatIDCannotChange(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)

	v := f.rejected(t, SetManagerPatch(ManagerPatch{
		ID: "tg", TargetChatID: int64ptr(-200),
	}))
	assert.Contains(t, v.Reason(), "forum target cannot change")
}

func TestSetManagerPatch_PrivateUserCannotChange(t *testing.T) {
	privateConfig := `providers:
    work:
        driver: anthropic
        api_key: ${WORK_API_KEY}
managers:
    - id: tg
      driver: telegram
      enabled: true
      bot_token: ${MANAGER_TG_BOT_TOKEN}
      allowed_user_ids:
        - 7
`
	f := newFixture(t, privateConfig, baseSecrets)

	v := f.rejected(t, SetManagerPatch(ManagerPatch{
		ID: "tg", AllowedUserIDs: &[]int64{8},
	}))
	assert.Contains(t, v.Reason(), "forum target cannot change")
}

func TestSetManagerPatch_CreationNeedsDriver(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)

	v := f.rejected(t, SetManagerPatch(ManagerPatch{
		ID: "tg2", BotToken: strptr(Ref("TOK")),
		AllowedUserIDs: &[]int64{7},
	}))
	assert.Contains(t, v.Reason(), "needs a driver")
}

func TestSetManagerPatch_CreationNeedsToken(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)

	v := f.rejected(t, SetManagerPatch(ManagerPatch{
		ID: "tg2", Driver: strptr("telegram"),
		AllowedUserIDs: &[]int64{7},
	}))
	assert.Contains(t, v.Reason(), "bot_token reference")
}

func TestSetManagerPatch_EnabledDefaultsToTrueOnCreate(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)

	_, v := f.svc.SetSecret("MANAGER_TG2_BOT_TOKEN", "9999:another-token-value")
	require.True(t, v.Applied)

	cfg := f.applied(t, SetManagerPatch(ManagerPatch{
		ID: "tg2", Driver: strptr("telegram"), BotToken: strptr(Ref("MANAGER_TG2_BOT_TOKEN")),
		AllowedUserIDs: &[]int64{7}, TargetChatID: int64ptr(-1001),
	}))

	require.Len(t, cfg.Managers, 2)
	require.NotNil(t, cfg.Managers[1].Enabled)
	assert.True(t, *cfg.Managers[1].Enabled)
}

func TestSetManagerPatch_NoOpDoesNotWrite(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)

	before := f.raw(t).Managers[0]
	f.applied(t, SetManagerPatch(ManagerPatch{
		ID: "tg", Driver: strptr("telegram"),
	}))
	after := f.raw(t).Managers[0]

	assert.Equal(t, before, after)
}

func TestSetManagerPatch_CanDisableExistingManager(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)

	cfg := f.applied(t, SetManagerPatch(ManagerPatch{
		ID: "tg", Enabled: boolptr(false),
	}))

	require.NotNil(t, cfg.Managers[0].Enabled)
	assert.False(t, *cfg.Managers[0].Enabled)
}
