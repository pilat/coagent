package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
)

func TestLoadSecrets(t *testing.T) {
	t.Run("reads values without touching the process environment", func(t *testing.T) {
		withSecretsFile(t, "COAGENT_AUDIT_SECRET=sentinel\n")

		secrets, err := LoadSecrets()
		require.NoError(t, err)
		assert.Equal(t, "sentinel", secrets["COAGENT_AUDIT_SECRET"])
		assert.Empty(t, os.Getenv("COAGENT_AUDIT_SECRET"))
	})

	t.Run("missing file yields an empty set", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())

		secrets, err := LoadSecrets()
		require.NoError(t, err)
		assert.Empty(t, secrets)
	})

	t.Run("malformed file is fatal", func(t *testing.T) {
		withSecretsFile(t, "this is not a key=value file\x00\n\"unterminated\n")

		_, err := LoadSecrets()
		require.Error(t, err)
	})
}

func TestSecretsEnvironment(t *testing.T) {
	t.Run("process environment wins over the secrets file", func(t *testing.T) {
		t.Setenv("COAGENT_AUDIT_KNOB", "from-environment")

		merged := Secrets{"COAGENT_AUDIT_KNOB": "from-file"}.Environment()
		assert.Equal(t, "from-environment", merged["COAGENT_AUDIT_KNOB"])
	})

	t.Run("secrets fill gaps in the process environment", func(t *testing.T) {
		merged := Secrets{"COAGENT_AUDIT_ONLY_IN_FILE": "from-file"}.Environment()
		assert.Equal(t, "from-file", merged["COAGENT_AUDIT_ONLY_IN_FILE"])
	})
}

// A credential in the secrets file must never become inheritable by tool
// subprocesses, while still reaching the config that needs it.
func TestNewConfigKeepsSecretsOutOfEnvironment(t *testing.T) {
	home := withSecretsFile(t, "COAGENT_AUDIT_SECRET=sentinel\n")

	writeHomeConfig(t, home, `
providers:
  anthropic:
    driver: anthropic
    api_key: ${COAGENT_AUDIT_SECRET}
`)

	cfg, _, err := NewConfig()
	require.NoError(t, err)

	assert.Equal(t, "sentinel", cfg.UnifiedConfig.Providers["anthropic"].APIKey)
	assert.Empty(t, os.Getenv("COAGENT_AUDIT_SECRET"))
	assert.NotContains(t, os.Environ(), "COAGENT_AUDIT_SECRET=sentinel")
}

func withSecretsFile(t *testing.T, content string) string {
	t.Helper()

	home := t.TempDir()
	coagentDir := filepath.Join(home, coagenthome.DirName)
	require.NoError(t, os.MkdirAll(coagentDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(coagentDir, coagenthome.SecretsFileName), []byte(content), 0o600))
	t.Setenv("HOME", home)

	return home
}

func writeHomeConfig(t *testing.T, home, content string) {
	t.Helper()

	path := filepath.Join(home, coagenthome.DirName, coagenthome.ConfigFileName)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

//nolint:gosec // fake credentials
func TestSecretValues(t *testing.T) {
	secrets := Secrets{
		"BOT_TOKEN": "8288787998:AAGlongenoughtoken",
		"SHORT":     "abc",
		"EMPTY":     "",
	}
	unified := &UnifiedConfig{
		Providers: map[string]ProviderEntry{
			"anthropic": {APIKey: "sk-ant-inline-key"},
			"dup":       {APIKey: "8288787998:AAGlongenoughtoken"},
			"nokey":     {},
		},
		Managers: []ManagerEntry{
			{BotToken: "inline-bot-token"},
		},
	}

	values := secretValues(secrets, unified)

	assert.ElementsMatch(t, []string{
		"8288787998:AAGlongenoughtoken",
		"sk-ant-inline-key",
		"inline-bot-token",
	}, values)
}

func TestSecretValuesNilUnified(t *testing.T) {
	values := secretValues(Secrets{"KEY": "longenoughsecret"}, nil)

	assert.Equal(t, []string{"longenoughsecret"}, values)
}
