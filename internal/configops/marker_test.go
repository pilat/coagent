package configops

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
)

var errEnrichment = errors.New("model catalog: claude-sonnet-5 is not in the anthropic catalog")

func markerFile(f *fixture) string {
	return filepath.Join(filepath.Dir(f.configPath), coagenthome.PendingApplyFileName)
}

// commit stages a document and commits it with a marker, as an apply does.
func (f *fixture) commit(t *testing.T, candidate string, p Pending) {
	t.Helper()

	staged := f.stageDocument(t, candidate)
	require.True(t, f.svc.Commit(staged, p).Applied)
}

func TestCommit_WritesTheMarkerNamingTheBackupAndTheNewHash(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)

	f.commit(t, `providers:
    work:
        driver: anthropic
        api_key: ${WORK_API_KEY}
models:
    - id: claude-sonnet-5
      provider: work
`, Pending{
		SessionID: 42,
	})

	p, err := f.svc.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, p)

	assert.Equal(t, int64(42), p.SessionID)

	// The backup names the file that was live, and the hash the one that landed.
	require.NotEmpty(t, p.BakPath)
	bak, err := os.ReadFile(p.BakPath)
	require.NoError(t, err)
	assert.Equal(t, baseConfig, string(bak))

	hash, err := f.svc.ConfigHash()
	require.NoError(t, err)
	assert.Equal(t, hash, p.NewHash)
}

func TestResolvePending_AppliedWhenTheBootSucceeds(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)
	f.commit(t, singleModelDocument, Pending{SessionID: 7})

	p, err := f.svc.LoadPending()
	require.NoError(t, err)

	out, err := f.svc.ResolvePending(*p, nil)
	require.NoError(t, err)

	assert.True(t, out.Verdict.Applied)
	assert.False(t, out.RolledBack)
	assert.Equal(t, int64(7), out.Pending.SessionID)

	// Boot owns clearing after resolution, including successful starts.
	assert.FileExists(t, markerFile(f))
	require.NoError(t, f.svc.ClearPending(*p))
	assert.NoFileExists(t, markerFile(f))
}

// The real trigger for a rollback is a failure pre-write validation cannot see —
// catalog enrichment, most of all.
func TestResolvePending_RollsBackWhenTheBootFails(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)
	f.commit(t, singleModelDocument, Pending{SessionID: 7})

	p, err := f.svc.LoadPending()
	require.NoError(t, err)

	out, err := f.svc.ResolvePending(*p, errEnrichment)
	require.NoError(t, err)

	assert.True(t, out.Verdict.Failed())
	assert.True(t, out.RolledBack)
	assert.Contains(t, out.Verdict.Reason(), "rolled back")
	assert.Contains(t, out.Verdict.Reason(), "not in the anthropic catalog")

	assert.Equal(t, baseConfig, f.configBytes(t), "the daemon boots on the config that worked")
	assert.FileExists(t, markerFile(f), "ResolvePending leaves the marker for the caller to clear")
}

// A crash between the marker and the write leaves a config the marker's hash does
// not describe. Nothing to roll back — the live file is still the working one.
func TestResolvePending_HashMismatchMeansTheWriteNeverLanded(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)

	out, err := f.svc.ResolvePending(Pending{SessionID: 7, NewHash: "deadbeef"}, nil)
	require.NoError(t, err)

	assert.True(t, out.Verdict.Failed())
	assert.False(t, out.RolledBack)
	assert.Contains(t, out.Verdict.Reason(), "config unchanged")
	assert.Equal(t, baseConfig, f.configBytes(t))
}

// The very first config has no backup: going back means having no config again,
// which is a legal state.
func TestResolvePending_RollsBackTheFirstConfigByRemovingIt(t *testing.T) {
	f := newFixture(t, "", "")

	f.commit(t, `providers:
    work:
        driver: anthropic
        api_key: sk-ant-first-00000000
models:
    - id: claude-sonnet-5
      provider: work
`, Pending{})

	p, err := f.svc.LoadPending()
	require.NoError(t, err)
	require.Empty(t, p.BakPath)

	out, err := f.svc.ResolvePending(*p, errEnrichment)
	require.NoError(t, err)

	assert.True(t, out.RolledBack)
	assert.NoFileExists(t, f.configPath)
}

func TestLoadPending_AbsentMarkerIsNotAnError(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)

	p, err := f.svc.LoadPending()
	require.NoError(t, err)
	assert.Nil(t, p, "no marker is the normal boot")
}

// Clearing an older resolution must not disarm a newer apply's rollback.
func TestClearPending_OnlyRemovesTheMarkerItResolved(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)
	f.commit(t, singleModelDocument, Pending{SessionID: 1})

	first, err := f.svc.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, first)

	f.commit(t, twoModelDocument, Pending{SessionID: 2})

	require.NoError(t, f.svc.ClearPending(*first))

	current, err := f.svc.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, current, "the newer apply retains its rollback marker")
	assert.Equal(t, int64(2), current.SessionID)
	wrongHash := *current
	wrongHash.NewHash = "different-hash"
	require.NoError(t, f.svc.ClearPending(wrongHash))
	assert.FileExists(t, markerFile(f))

	data, err := json.Marshal(map[string]any{
		"session_id": current.SessionID, "bak_path": current.BakPath, "new_config_sha256": current.NewHash,
		"tool_call_id": "old-call", "tool_name": "config_edit", "summary": "replace configuration",
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(markerFile(f), data, 0o600))

	require.NoError(t, f.svc.ClearPending(*current))
	assert.NoFileExists(t, markerFile(f))
}

// A bootstrap op has no session waiting: the marker still drives the rollback,
// there is simply nobody to tell.
func TestResolvePending_BootstrapMarkerCarriesNoSession(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)
	f.commit(t, singleModelDocument, Pending{})

	p, err := f.svc.LoadPending()
	require.NoError(t, err)

	out, err := f.svc.ResolvePending(*p, errEnrichment)
	require.NoError(t, err)

	assert.Equal(t, int64(0), out.Pending.SessionID)
	assert.True(t, out.RolledBack)
}

const singleModelDocument = `providers:
    work:
        driver: anthropic
        api_key: ${WORK_API_KEY}
models:
    - id: claude-sonnet-5
      provider: work
`

const twoModelDocument = `providers:
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
`
