package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
)

func TestNewConfig(t *testing.T) {
	t.Run("returns config with valid structure", func(t *testing.T) {
		// Redirect HOME to temp dir so NewConfig doesn't load the user's real config
		tmpDir := t.TempDir()
		coagentDir := filepath.Join(tmpDir, coagenthome.DirName)
		require.NoError(t, os.MkdirAll(coagentDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(coagentDir, coagenthome.ConfigFileName), []byte("{}"), 0o644))
		t.Setenv("HOME", tmpDir)

		cfg, _, err := NewConfig()
		require.NoError(t, err)
		require.NotNil(t, cfg)

		_ = cfg.Model
		_ = cfg.SubagentModel
	})

	t.Run("loads subagent model env var", func(t *testing.T) {
		tmpDir := t.TempDir()
		coagentDir := filepath.Join(tmpDir, coagenthome.DirName)
		require.NoError(t, os.MkdirAll(coagentDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(coagentDir, coagenthome.ConfigFileName), []byte("{}"), 0o644))
		t.Setenv("HOME", tmpDir)
		t.Setenv("SUBAGENT_MODEL", "test-subagent-model")

		cfg, _, err := NewConfig()
		require.NoError(t, err)

		assert.Equal(t, "test-subagent-model", cfg.SubagentModel)
	})
}
