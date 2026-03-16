package configops

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
)

// The dotenv reader resolves duplicates last-wins, so rotating only the first
// assignment leaves the daemon holding the stale credential.
func TestSetSecret_RotatesEveryAssignmentOfTheSameKey(t *testing.T) {
	//nolint:gosec // fake credentials
	const secrets = `# hand-written note the machine must not eat
WORK_API_KEY=sk-ant-first-000000000
OTHER=keep-me-000000000
WORK_API_KEY=sk-ant-stale-000000000
`

	f := newFixture(t, baseConfig, secrets)

	referenced, v := f.svc.SetSecret("WORK_API_KEY", "sk-ant-rotated-00000000")
	require.True(t, v.Applied, v.Reason())
	assert.True(t, referenced)

	got, err := config.LoadSecretsFrom(f.secretPath)
	require.NoError(t, err)
	assert.Equal(t, "sk-ant-rotated-00000000", got["WORK_API_KEY"], "the rotation is what the reader resolves")
	assert.Equal(t, "keep-me-000000000", got["OTHER"])

	body := f.secretsBytes(t)
	assert.Equal(t, 1, strings.Count(body, "WORK_API_KEY"), "exactly one assignment survives")
	assert.Contains(t, body, "# hand-written note the machine must not eat")
}

// Every assignment form the reader accepts has to be recognised by the editor,
// or the rotation appends a duplicate that the old value then outlives.
func TestSetSecret_RotatesEveryFormTheReaderAccepts(t *testing.T) {
	tests := map[string]string{
		"spaced":         "WORK_API_KEY = sk-ant-old-0000000000\n",
		"indented":       "  WORK_API_KEY=sk-ant-old-0000000000\n",
		"export spaced":  "export WORK_API_KEY = sk-ant-old-0000000000\n",
		"trailing space": "WORK_API_KEY=sk-ant-old-0000000000  \n",
	}

	for name, secrets := range tests {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, baseConfig, secrets+"OTHER=keep-me-000000000\n")

			_, v := f.svc.SetSecret("WORK_API_KEY", "sk-ant-new-0000000000")
			require.True(t, v.Applied, v.Reason())

			got, err := config.LoadSecretsFrom(f.secretPath)
			require.NoError(t, err)
			assert.Equal(t, "sk-ant-new-0000000000", got["WORK_API_KEY"])
			assert.Equal(t, "keep-me-000000000", got["OTHER"])

			assert.Equal(t, 1, strings.Count(f.secretsBytes(t), "WORK_API_KEY"),
				"rotated in place, not duplicated")
		})
	}
}

// A key that only prefixes another one is a different variable.
func TestSetSecret_LeavesANeighbourWithASharedPrefixAlone(t *testing.T) {
	f := newFixture(t, baseConfig, "WORK_API_KEY_OLD=sk-ant-old-0000000000\n")

	_, v := f.svc.SetSecret("WORK_API_KEY", "sk-ant-new-0000000000")
	require.True(t, v.Applied, v.Reason())

	got, err := config.LoadSecretsFrom(f.secretPath)
	require.NoError(t, err)
	assert.Equal(t, "sk-ant-old-0000000000", got["WORK_API_KEY_OLD"])
	assert.Equal(t, "sk-ant-new-0000000000", got["WORK_API_KEY"])
}
