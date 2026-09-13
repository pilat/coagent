package configops

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStageDocument_ValidCandidateStagesWithoutWriting(t *testing.T) {
	t.Parallel()

	f := newFixture(t, baseConfig, baseSecrets)

	candidate := `providers:
    work:
        driver: anthropic
        api_key: ${WORK_API_KEY}
models:
    - id: claude-sonnet-5
      provider: work
`
	staged, v := f.svc.StageDocument([]byte(candidate))
	require.True(t, v.Applied, "stage rejected: %s", v.Reason())
	require.NotNil(t, staged)
	assert.NotEmpty(t, staged.Hash)
	assert.NotEmpty(t, staged.Summary)
	assert.Equal(t, candidate, string(staged.Data), "raw ${VAR} references stay intact")
	assert.NotContains(t, string(staged.Data), fakeKeyValue, "no credential value is copied")

	assert.Equal(t, baseConfig, f.configBytes(t), "staging leaves the on-disk config unchanged")
}

func TestStageDocument_RejectsInvalidCandidatesBeforeStaging(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		candidate string
		wantErr   string
	}{
		{
			name:      "malformed yaml",
			candidate: "providers: [unclosed",
		},
		{
			name: "unknown field",
			candidate: `providers:
    work:
        driver: anthropic
        api_key: ${WORK_API_KEY}
        unknow_field: 1
`,
		},
		{
			name:      "empty document",
			candidate: "   \n",
		},
		{
			name: "model references unknown provider",
			candidate: `providers:
    work:
        driver: anthropic
        api_key: ${WORK_API_KEY}
models:
    - id: m1
      provider: missing
`,
		},
		{
			name: "duplicate model ids",
			candidate: `providers:
    work:
        driver: anthropic
        api_key: ${WORK_API_KEY}
models:
    - id: m1
      provider: work
    - id: m1
      provider: work
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, baseConfig, baseSecrets)

			staged, v := f.svc.StageDocument([]byte(tt.candidate))
			assert.False(t, v.Applied)
			assert.Nil(t, staged)
			assert.Equal(t, baseConfig, f.configBytes(t), "a rejected candidate writes nothing")
		})
	}
}

func TestStageDocument_RejectsUnresolvedSecretReference(t *testing.T) {
	t.Parallel()

	f := newFixture(t, baseConfig, baseSecrets)

	candidate := `providers:
    work:
        driver: anthropic
        api_key: ${MISSING_API_KEY}
models:
    - id: claude-sonnet-5
      provider: work
`
	staged, v := f.svc.StageDocument([]byte(candidate))
	assert.False(t, v.Applied)
	assert.Nil(t, staged)
	assert.Equal(t, baseConfig, f.configBytes(t))
}

func TestStageDocument_TypedOpsUnchanged(t *testing.T) {
	t.Parallel()

	f := newFixture(t, baseConfig, baseSecrets)
	staged, v := f.svc.Stage(SetDefaultModel("claude-sonnet-5"))
	require.True(t, v.Applied, "%s", v.Reason())
	require.True(t, f.svc.Commit(staged, Pending{}).Applied)

	cfg := f.raw(t)
	require.NotEmpty(t, cfg.Models)
	assert.Equal(t, "claude-sonnet-5", cfg.Models[0].ID)
}
