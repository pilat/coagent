package configops

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
)

func TestStageDocument_Browser(t *testing.T) {
	t.Parallel()

	for _, section := range []string{
		"browser:\n  mcp_name: remote\n",
		"browser:\n",
		"browser: ~\n",
		"browser: {}\n",
		"browser:\n  mcp_name: ''\n",
		"browser:\n  mcp_name: ' remote '\n",
	} {
		t.Run(section, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, baseConfig, baseSecrets)
			candidate := baseConfig + "\n" + section
			staged, verdict := f.svc.StageDocument([]byte(candidate))
			if section != "browser:\n  mcp_name: remote\n" {
				require.True(t, verdict.Failed())
				assert.Contains(t, verdict.Reason(), "browser")
				assert.Nil(t, staged)
				assert.Equal(t, baseConfig, f.configBytes(t))
				return
			}
			require.True(t, verdict.Applied, "%s", verdict.Reason())
			assert.Equal(t, candidate, string(staged.Data))
			require.True(t, f.svc.Commit(staged, Pending{}).Applied)
			secrets, err := config.LoadSecretsFrom(f.secretPath)
			require.NoError(t, err)
			cfg, err := config.LoadUnifiedConfig(f.configPath, secrets)
			require.NoError(t, err)
			assert.Equal(t, &config.BrowserConfig{MCPName: "remote"}, cfg.Browser)
		})
	}
}
