package configops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/logger"
)

// baseConfig is a valid two-provider, two-model, one-manager config in raw form
// — every credential a ${VAR}, as the file on disk always is.
const baseConfig = `providers:
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
`

// Fixture credential values. They live in consts so the literals sit in one
// place a reader can check, rather than scattered through the table.
const (
	fakeKeyValue = "sk-ant-a-real-looking-key"
	fakeBareRef  = "$SECOND_API_KEY"
	fakeBotToken = "1234:AAH-real-token"
)

//nolint:gosec // fake credentials
const baseSecrets = `# hand-written note the machine must not eat
WORK_API_KEY=sk-ant-work-0000000000
ROUTER_API_KEY=sk-or-router-000000000
MANAGER_TG_BOT_TOKEN=1234:token-value-here
`

type fixture struct {
	svc        Service
	configPath string
	secretPath string
}

func newFixture(t *testing.T, configYAML, secrets string) *fixture {
	t.Helper()

	dir := t.TempDir()
	f := &fixture{
		configPath: filepath.Join(dir, "config.yaml"),
		secretPath: filepath.Join(dir, "secrets"),
	}

	if configYAML != "" {
		require.NoError(t, os.WriteFile(f.configPath, []byte(configYAML), 0o600))
	}

	if secrets != "" {
		require.NoError(t, os.WriteFile(f.secretPath, []byte(secrets), 0o600))
	}

	f.svc = New(f.configPath, f.secretPath)

	return f
}

// applied stages and commits, failing the test if either half rejects.
func (f *fixture) applied(t *testing.T, op Op) *config.UnifiedConfig {
	t.Helper()

	staged, v := f.svc.Stage(op)
	require.True(t, v.Applied, "stage rejected: %s", v.Reason())
	require.True(t, f.svc.Commit(staged, Pending{}).Applied)

	return f.raw(t)
}

func (f *fixture) rejected(t *testing.T, op Op) Verdict {
	t.Helper()

	staged, v := f.svc.Stage(op)
	require.False(t, v.Applied, "expected a rejection")
	assert.Nil(t, staged)

	return v
}

func (f *fixture) raw(t *testing.T) *config.UnifiedConfig {
	t.Helper()

	cfg, err := config.LoadRawUnifiedConfig(f.configPath)
	require.NoError(t, err)

	return cfg
}

func (f *fixture) configBytes(t *testing.T) string {
	t.Helper()

	data, err := os.ReadFile(f.configPath)
	require.NoError(t, err)

	return string(data)
}

func (f *fixture) secretsBytes(t *testing.T) string {
	t.Helper()

	data, err := os.ReadFile(f.secretPath)
	require.NoError(t, err)

	return string(data)
}

func modelIDs(cfg *config.UnifiedConfig) []string {
	out := make([]string, 0, len(cfg.Models))
	for _, m := range cfg.Models {
		out = append(out, m.ID)
	}

	return out
}

func TestStage_RejectsCredentialValueInEveryCredentialField(t *testing.T) {
	tests := []struct {
		name string
		op   Op
	}{
		{
			name: "provider api_key",
			op: SetProvider("second", config.ProviderEntry{
				Driver: "anthropic", APIKey: fakeKeyValue,
			}),
		},
		{
			name: "manager bot_token",
			op: SetManager(config.ManagerEntry{
				ID: "tg2", Driver: "telegram", BotToken: fakeBotToken,
				AllowedUserIDs: []int64{7}, TargetChatID: -100,
			}),
		},
		{
			name: "a bare $VAR is not the braced form the loader resolves",
			op: SetProvider("second", config.ProviderEntry{
				Driver: "anthropic", APIKey: fakeBareRef,
			}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, baseConfig, baseSecrets)
			v := f.rejected(t, tt.op)
			assert.Contains(t, v.Reason(), refOnlyMessage)
			assert.Equal(t, baseConfig, f.configBytes(t), "a rejected op writes nothing")
		})
	}
}

func TestSetProvider_NewProviderNeedsAKeyWhenTheDriverDemandsOne(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)

	v := f.rejected(t, SetProvider("second", config.ProviderEntry{Driver: "anthropic"}))
	assert.Contains(t, v.Reason(), "needs an api_key reference")
}

func TestSetProvider_KeylessDriverNeedsNoKey(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)

	cfg := f.applied(t, SetProvider("vertex", config.ProviderEntry{
		Driver: "google-sa", SAFile: "/etc/coagent/sa.json", BaseURL: "https://example.invalid",
	}))
	assert.Equal(t, "google-sa", cfg.Providers["vertex"].Driver)
}

func TestSetProvider_EmptyKeyKeepsTheExistingReference(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)

	cfg := f.applied(t, SetProvider("work", config.ProviderEntry{Driver: "anthropic", Catalog: "anthropic"}))
	assert.Equal(t, "${WORK_API_KEY}", cfg.Providers["work"].APIKey)
	assert.Equal(t, "anthropic", cfg.Providers["work"].Catalog)
}

func TestRemoveProvider_Guards(t *testing.T) {
	t.Run("refuses the last provider", func(t *testing.T) {
		single := `providers:
    only:
        driver: anthropic
        api_key: ${WORK_API_KEY}
`
		f := newFixture(t, single, baseSecrets)

		v := f.rejected(t, RemoveProvider("only"))
		assert.Contains(t, v.Reason(), "only provider")
	})

	t.Run("refuses a provider models still reference, naming them", func(t *testing.T) {
		f := newFixture(t, baseConfig, baseSecrets)

		v := f.rejected(t, RemoveProvider("router"))
		assert.Contains(t, v.Reason(), "anthropic/claude-sonnet-5")
		assert.Contains(t, v.Reason(), "remove those models first")
	})

	t.Run("removes a provider nothing references", func(t *testing.T) {
		f := newFixture(t, baseConfig, baseSecrets)

		require.True(
			t,
			f.svc.Commit(mustStage(t, f.svc, RemoveModel("anthropic/claude-sonnet-5", "")), Pending{}).Applied,
		)

		cfg := f.applied(t, RemoveProvider("router"))
		assert.NotContains(t, cfg.Providers, "router")
		assert.Contains(t, cfg.Providers, "work")
	})
}

func TestRemoveModel_DefaultNeedsAReplacement(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)

	v := f.rejected(t, RemoveModel("claude-sonnet-5", ""))
	assert.Contains(t, v.Reason(), "name its replacement")

	v = f.rejected(t, RemoveModel("claude-sonnet-5", "claude-sonnet-5"))
	assert.Contains(t, v.Reason(), "cannot be the model being removed")

	v = f.rejected(t, RemoveModel("claude-sonnet-5", "gpt-nope"))
	assert.Contains(t, v.Reason(), `no model named "gpt-nope"`)

	cfg := f.applied(t, RemoveModel("claude-sonnet-5", "anthropic/claude-sonnet-5"))
	assert.Equal(t, []string{"anthropic/claude-sonnet-5"}, modelIDs(cfg))
}

func TestSetDefaultModel_ReordersToIndexZero(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)

	cfg := f.applied(t, SetDefaultModel("anthropic/claude-sonnet-5"))
	assert.Equal(t, []string{"anthropic/claude-sonnet-5", "claude-sonnet-5"}, modelIDs(cfg))

	v := f.rejected(t, SetDefaultModel("nope"))
	assert.Contains(t, v.Reason(), `no model named "nope"`)
}

func TestSetModelTags_ReplacesAndDeduplicates(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)
	cfg := f.applied(t, SetModelTags("claude-sonnet-5", []string{"coding", "fast", "coding"}))
	assert.Equal(t, []string{"coding", "fast"}, cfg.Models[0].Tags)

	cfg = f.applied(t, SetModelTags("claude-sonnet-5", nil))
	assert.Empty(t, cfg.Models[0].Tags)

	v := f.rejected(t, SetModelTags("unknown", []string{"fast"}))
	assert.Contains(t, v.Reason(), `no model named "unknown"`)
	v = f.rejected(t, SetModelTags("claude-sonnet-5", []string{"Not valid"}))
	assert.Contains(t, v.Reason(), "invalid tag")
}

func TestAddModel_Guards(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)

	v := f.rejected(t, AddModel(config.ModelEntry{ID: "x", Provider: "ghost"}))
	assert.Contains(t, v.Reason(), `no provider named "ghost"`)

	v = f.rejected(t, AddModel(config.ModelEntry{ID: "claude-sonnet-5", Provider: "work"}))
	assert.Contains(t, v.Reason(), "already configured")

	cfg := f.applied(t, AddModel(config.ModelEntry{ID: "claude-opus-5", Provider: "work"}))
	assert.Equal(t, "claude-sonnet-5", cfg.Models[0].ID, "an appended model never becomes the default")
	assert.Len(t, cfg.Models, 3)
}

func TestSetManager_SetsEnabledExplicitly(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)

	_, v := f.svc.SetSecret("MANAGER_TG2_BOT_TOKEN", "9999:another-token-value")
	require.True(t, v.Applied)

	cfg := f.applied(t, SetManager(config.ManagerEntry{
		ID: "tg2", Driver: "telegram", BotToken: Ref("MANAGER_TG2_BOT_TOKEN"),
		AllowedUserIDs: []int64{7}, TargetChatID: -1001,
	}))

	require.Len(t, cfg.Managers, 2)
	require.NotNil(t, cfg.Managers[1].Enabled)
	assert.True(t, *cfg.Managers[1].Enabled)
	assert.Contains(t, f.configBytes(t), "enabled: true")
}

func TestRemoveManager(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)

	v := f.rejected(t, RemoveManager("ghost"))
	assert.Contains(t, v.Reason(), `no manager named "ghost"`)

	cfg := f.applied(t, RemoveManager("tg"))
	assert.Empty(t, cfg.Managers)
}

// A staged draft must never carry a resolved credential into the file. This
// asserts on the bytes, because that is where the damage would be.
func TestCommit_NeverWritesAResolvedSecret(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)

	f.applied(t, SetDefaultModel("anthropic/claude-sonnet-5"))

	body := f.configBytes(t)
	for _, secret := range []string{"sk-ant-work-0000000000", "sk-or-router-000000000", "1234:token-value-here"} {
		assert.NotContains(t, body, secret)
	}

	assert.Contains(t, body, "${WORK_API_KEY}")
	assert.Contains(t, body, "${MANAGER_TG_BOT_TOKEN}")
}

// A ${VAR} written this second must already count: the bootstrap writes the
// secret and then the provider that references it, in the same breath.
func TestStage_SeesASecretWrittenAMomentAgo(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)

	entry := config.ProviderEntry{Driver: "anthropic", APIKey: Ref("SECOND_API_KEY")}

	v := f.rejected(t, SetProvider("second", entry))
	assert.Contains(t, v.Reason(), "undefined SECOND_API_KEY")

	referenced, sv := f.svc.SetSecret("SECOND_API_KEY", "sk-ant-second-0000000")
	require.True(t, sv.Applied)
	assert.False(t, referenced, "nothing references it yet — this is the onboarding case")

	cfg := f.applied(t, SetProvider("second", entry))
	assert.Equal(t, "${SECOND_API_KEY}", cfg.Providers["second"].APIKey)
}

func TestSetSecret_EditsInPlaceAndLeavesTheRestAlone(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)

	referenced, v := f.svc.SetSecret("WORK_API_KEY", "sk-ant-work-rotated-1")
	require.True(t, v.Applied)
	assert.True(t, referenced, "a rotation of a referenced var must trigger a restart")

	body := f.secretsBytes(t)
	assert.Contains(t, body, "# hand-written note the machine must not eat")
	assert.Contains(t, body, "WORK_API_KEY=sk-ant-work-rotated-1")
	assert.NotContains(t, body, "sk-ant-work-0000000000")
	assert.Contains(t, body, "ROUTER_API_KEY=sk-or-router-000000000")
	assert.Equal(t, 1, strings.Count(body, "WORK_API_KEY="))
}

func TestSetSecret_RejectsBadNames(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)

	_, v := f.svc.SetSecret("not a var", "value")
	assert.Contains(t, v.Reason(), "AN_ENV_VAR")

	_, v = f.svc.SetSecret("EMPTY", "")
	assert.Contains(t, v.Reason(), "needs a value")
}

// A credential is scrubbed from logs from the moment it is written, not from the
// next boot.
func TestSetSecret_RegistersRedactionImmediately(t *testing.T) {
	t.Cleanup(func() { logger.SetRedactedValues(nil) })

	f := newFixture(t, baseConfig, baseSecrets)
	const value = "sk-ant-must-never-be-logged"

	assert.Contains(t, logger.Redact("key is "+value), value)

	_, v := f.svc.SetSecret("LOGGABLE", value)
	require.True(t, v.Applied)

	assert.Equal(t, "key is [REDACTED]", logger.Redact("key is "+value))
}

// A secrets write must never touch config.yaml: a crash between the two leaves
// an orphan credential nothing references, never a config with a dangling ${VAR}.
func TestSetSecret_DoesNotTouchTheConfig(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)

	_, v := f.svc.SetSecret("SOMETHING_NEW", "sk-value-000000000000")
	require.True(t, v.Applied)

	assert.Equal(t, baseConfig, f.configBytes(t))
}

// Every value the reader can give back must come back byte for byte, and the
// ones it cannot are refused rather than silently mangled.
func TestSetSecret_ValueRoundTrips(t *testing.T) {
	roundTrips := []string{
		"sk-ant-plain-000000000",
		"1234:AAH-token_with-dashes",
		"has spaces in it",
		"trailing # not a comment",
		"literal ${NOT_EXPANDED} braces",
		`back\slash and "double quotes"`,
	}

	for _, value := range roundTrips {
		t.Run(value, func(t *testing.T) {
			f := newFixture(t, baseConfig, baseSecrets)

			_, v := f.svc.SetSecret("PROBE", value)
			require.True(t, v.Applied, v.Reason())

			got, err := config.LoadSecretsFrom(f.secretPath)
			require.NoError(t, err)
			assert.Equal(t, value, got["PROBE"])
			assert.Equal(t, "sk-or-router-000000000", got["ROUTER_API_KEY"], "neighbours are untouched")
		})
	}

	refused := map[string]string{
		"it's quoted":  "single quote",
		"two\nlines":   "single line",
		"carriage\rre": "single line",
	}

	for value, want := range refused {
		f := newFixture(t, baseConfig, baseSecrets)

		_, v := f.svc.SetSecret("PROBE", value)
		require.False(t, v.Applied)
		assert.Contains(t, v.Reason(), want)
	}
}

func TestSetSecret_RotatesTheExportForm(t *testing.T) {
	f := newFixture(t, baseConfig, "export WORK_API_KEY=sk-ant-old-0000000000\nOTHER=keep-me-000000000\n")

	referenced, v := f.svc.SetSecret("WORK_API_KEY", "sk-ant-new-0000000000")
	require.True(t, v.Applied)
	assert.True(t, referenced)

	body := f.secretsBytes(t)
	assert.Equal(t, 1, strings.Count(body, "WORK_API_KEY="), "rotated in place, not duplicated")
	assert.Contains(t, body, "OTHER=keep-me-000000000")

	got, err := config.LoadSecretsFrom(f.secretPath)
	require.NoError(t, err)
	assert.Equal(t, "sk-ant-new-0000000000", got["WORK_API_KEY"])
}

func TestStage_RefusesAnOpThatWouldProduceAnUnloadableFile(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)

	// openrouter without base_url is rejected by the strict loader; the op layer
	// never writes bytes it has not proven load.
	v := f.rejected(t, SetProvider("router", config.ProviderEntry{Driver: "openrouter"}))
	assert.Contains(t, v.Reason(), "base_url")
	assert.Equal(t, baseConfig, f.configBytes(t))
}

func TestCommit_OnAMissingConfigWritesTheFirstFile(t *testing.T) {
	f := newFixture(t, "", "")

	_, v := f.svc.SetSecret("FRESH_API_KEY", "sk-ant-fresh-00000000")
	require.True(t, v.Applied)

	cfg := f.applied(t, SetProvider("work", config.ProviderEntry{
		Driver: "anthropic", APIKey: Ref("FRESH_API_KEY"),
	}))
	assert.Equal(t, "anthropic", cfg.Providers["work"].Driver)

	// No previous file means no backup to take.
	entries, err := os.ReadDir(filepath.Dir(f.configPath))
	require.NoError(t, err)

	for _, e := range entries {
		assert.NotContains(t, e.Name(), backupSuffix)
	}
}

func TestCommit_BacksUpTheFileItReplaces(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)

	f.applied(t, SetDefaultModel("anthropic/claude-sonnet-5"))

	baks := backupNames(t, f.configPath)
	require.Len(t, baks, 1)

	data, err := os.ReadFile(filepath.Join(filepath.Dir(f.configPath), baks[0]))
	require.NoError(t, err)
	assert.Equal(t, baseConfig, string(data), "the backup is the file that was live")
}

func TestPruneBackups_KeepsTheNewestTwenty(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)
	dir := filepath.Dir(f.configPath)

	for i := range 25 {
		name := f.configPath + backupSuffix + "20260808-" + twoDigits(i) + "0000"
		require.NoError(t, os.WriteFile(name, []byte("old"), 0o600))
	}

	// An unrelated file in the directory is the human's and must survive.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml.mine"), []byte("keep"), 0o600))

	pruneBackups(f.configPath)

	baks := backupNames(t, f.configPath)
	assert.Len(t, baks, backupRetention)
	assert.Equal(t, "config.yaml"+backupSuffix+"20260808-050000", baks[0], "the oldest five are gone")

	_, err := os.Stat(filepath.Join(dir, "config.yaml.mine"))
	require.NoError(t, err)
}

func TestUpperSnake(t *testing.T) {
	tests := map[string]string{
		"work":        "WORK",
		"my-provider": "MY_PROVIDER",
		"tg main":     "TG_MAIN",
		"2fast":       "V2FAST",
	}

	for in, want := range tests {
		assert.Equal(t, want, upperSnake(in), in)
	}

	assert.Equal(t, "WORK_API_KEY", SecretVarForProvider("work"))
	assert.Equal(t, "MANAGER_TG_BOT_TOKEN", secretVarForManager("tg"))
}

func mustStage(t *testing.T, s Service, op Op) *Staged {
	t.Helper()

	staged, v := s.Stage(op)
	require.True(t, v.Applied, v.Reason())

	return staged
}

func backupNames(t *testing.T, configPath string) []string {
	t.Helper()

	entries, err := os.ReadDir(filepath.Dir(configPath))
	require.NoError(t, err)

	prefix := filepath.Base(configPath) + backupSuffix

	var out []string

	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			out = append(out, e.Name())
		}
	}

	return out
}

func twoDigits(i int) string {
	if i < 10 {
		return "0" + string(rune('0'+i))
	}

	return string(rune('0'+i/10)) + string(rune('0'+i%10))
}

// The manager-side mirror of the new-provider-key guard: a manager with no token
// to reference is a config the daemon refuses to start on.
func TestSetManager_NewManagerNeedsATokenReference(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)

	v := f.rejected(t, SetManager(config.ManagerEntry{
		ID: "tg2", Driver: "telegram", AllowedUserIDs: []int64{7}, TargetChatID: -100,
	}))
	assert.Contains(t, v.Reason(), "bot_token reference")
}

// An empty token on an *existing* manager keeps the one it already had, the same
// way an empty key does for a provider.
func TestSetManager_EmptyTokenKeepsTheExistingReference(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)

	cfg := f.applied(t, SetManager(config.ManagerEntry{
		ID: "tg", Driver: "telegram", AllowedUserIDs: []int64{7, 9}, TargetChatID: -200,
	}))

	require.Len(t, cfg.Managers, 1)
	assert.Equal(t, "${MANAGER_TG_BOT_TOKEN}", cfg.Managers[0].BotToken)
	assert.Equal(t, []int64{7, 9}, cfg.Managers[0].AllowedUserIDs)
	assert.Equal(t, int64(-200), cfg.Managers[0].TargetChatID)
}

// A new provider missing its key is refused for every driver whose schema
// demands one, not just the one that happened to get a test.
func TestSetProvider_KeyGuardCoversEveryDriverThatNeedsOne(t *testing.T) {
	for _, driver := range []string{"anthropic", "openai", "openrouter"} {
		t.Run(driver, func(t *testing.T) {
			f := newFixture(t, baseConfig, baseSecrets)

			v := f.rejected(t, SetProvider("second", config.ProviderEntry{
				Driver: driver, BaseURL: "https://example.invalid",
			}))
			assert.Contains(t, v.Reason(), "needs an api_key reference")
		})
	}
}
