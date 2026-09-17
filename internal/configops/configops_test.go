package configops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// stageDocument stages a whole document, failing the test on rejection.
func (f *fixture) stageDocument(t *testing.T, candidate string) *Staged {
	t.Helper()

	staged, v := f.svc.StageDocument([]byte(candidate))
	require.True(t, v.Applied, "stage rejected: %s", v.Reason())

	return staged
}

// applied stages a whole document and commits it, failing on either rejection.
func (f *fixture) applied(t *testing.T, candidate string) {
	t.Helper()

	staged := f.stageDocument(t, candidate)
	require.True(t, f.svc.Commit(staged, Pending{}).Applied)
}

func (f *fixture) configBytes(t *testing.T) string {
	t.Helper()

	data, err := os.ReadFile(f.configPath)
	require.NoError(t, err)

	return string(data)
}

// A staged document commits byte for byte, with ${VAR} references intact.
func TestCommit_WritesTheStagedDocument(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)

	candidate := `providers:
    work:
        driver: anthropic
        api_key: ${WORK_API_KEY}
models:
    - id: claude-sonnet-5
      provider: work
`
	f.applied(t, candidate)

	assert.Equal(t, candidate, f.configBytes(t))
}

// A staged draft must never carry a resolved credential into the file. This
// asserts on the bytes, because that is where the damage would be.
func TestCommit_NeverWritesAResolvedSecret(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)

	candidate := `providers:
    work:
        driver: anthropic
        api_key: ${WORK_API_KEY}
    router:
        driver: openrouter
        api_key: ${ROUTER_API_KEY}
        base_url: https://openrouter.ai/api/v1
models:
    - id: anthropic/claude-sonnet-5
      provider: router
    - id: claude-sonnet-5
      provider: work
managers:
    - id: tg
      driver: telegram
      enabled: true
      bot_token: ${MANAGER_TG_BOT_TOKEN}
      allowed_user_ids:
        - 7
      target_chat_id: -100
`
	f.applied(t, candidate)

	body := f.configBytes(t)
	for _, secret := range []string{"sk-ant-work-0000000000", "sk-or-router-000000000", "1234:token-value-here"} {
		assert.NotContains(t, body, secret)
	}

	assert.Contains(t, body, "${WORK_API_KEY}")
	assert.Contains(t, body, "${MANAGER_TG_BOT_TOKEN}")
}

func TestStageDocument_RefusesAnUnloadableCandidate(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)

	// openrouter without base_url is rejected by the strict loader; the staging
	// layer never writes bytes it has not proven load.
	candidate := `providers:
    router:
        driver: openrouter
        api_key: ${ROUTER_API_KEY}
models:
    - id: anthropic/claude-sonnet-5
      provider: router
`
	staged, v := f.svc.StageDocument([]byte(candidate))
	assert.False(t, v.Applied)
	assert.Nil(t, staged)
	assert.Contains(t, v.Reason(), "base_url")
	assert.Equal(t, baseConfig, f.configBytes(t))
}

func TestCommit_OnAMissingConfigWritesTheFirstFile(t *testing.T) {
	f := newFixture(t, "", "")

	f.applied(t, `providers:
    work:
        driver: anthropic
        api_key: sk-ant-fresh-00000000
models:
    - id: claude-sonnet-5
      provider: work
`)

	assert.Contains(t, f.configBytes(t), "driver: anthropic")

	// No previous file means no backup to take.
	entries, err := os.ReadDir(filepath.Dir(f.configPath))
	require.NoError(t, err)

	for _, e := range entries {
		assert.NotContains(t, e.Name(), backupSuffix)
	}
}

func TestCommit_BacksUpTheFileItReplaces(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)

	f.applied(t, `providers:
    work:
        driver: anthropic
        api_key: ${WORK_API_KEY}
models:
    - id: claude-sonnet-5
      provider: work
`)

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
