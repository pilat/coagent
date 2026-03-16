package configops

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
)

var errEnrichment = errors.New("model catalog: claude-sonnet-5 is not in the anthropic catalog")

func markerFile(f *fixture) string {
	return filepath.Join(filepath.Dir(f.configPath), coagenthome.PendingApplyFileName)
}

// commit stages an op and commits it with a marker, as an apply does.
func (f *fixture) commit(t *testing.T, op Op, p Pending) {
	t.Helper()

	staged, v := f.svc.Stage(op)
	require.True(t, v.Applied, v.Reason())
	require.True(t, f.svc.Commit(staged, p).Applied)
}

func TestCommit_WritesTheMarkerNamingTheBackupAndTheNewHash(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)

	f.commit(t, SetDefaultModel("anthropic/claude-sonnet-5"), Pending{
		SessionID: 42, ToolCallID: "call-1", ToolName: "set_default_model",
	})

	p, err := f.svc.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, p)

	assert.Equal(t, int64(42), p.SessionID)
	assert.Equal(t, "call-1", p.ToolCallID)
	assert.Equal(t, "set default model anthropic/claude-sonnet-5", p.Summary)

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
	f.commit(t, SetDefaultModel("anthropic/claude-sonnet-5"), Pending{SessionID: 7, ToolCallID: "c"})

	p, err := f.svc.LoadPending()
	require.NoError(t, err)

	out, err := f.svc.ResolvePending(*p, nil)
	require.NoError(t, err)

	assert.True(t, out.Verdict.Applied)
	assert.False(t, out.RolledBack)
	assert.Equal(t, int64(7), out.Pending.SessionID)

	// The waiting session has not been told yet, so the marker is still owed.
	assert.FileExists(t, markerFile(f))
	require.NoError(t, f.svc.ClearPending(*p))
	assert.NoFileExists(t, markerFile(f))
}

// The real trigger for a rollback is a failure pre-write validation cannot see —
// catalog enrichment, most of all.
func TestResolvePending_RollsBackWhenTheBootFails(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)
	f.commit(t, SetDefaultModel("anthropic/claude-sonnet-5"), Pending{SessionID: 7, ToolCallID: "c"})

	p, err := f.svc.LoadPending()
	require.NoError(t, err)

	out, err := f.svc.ResolvePending(*p, errEnrichment)
	require.NoError(t, err)

	assert.True(t, out.Verdict.Failed())
	assert.True(t, out.RolledBack)
	assert.Contains(t, out.Verdict.Reason(), "rolled back")
	assert.Contains(t, out.Verdict.Reason(), "not in the anthropic catalog")

	assert.Equal(t, baseConfig, f.configBytes(t), "the daemon boots on the config that worked")
	assert.FileExists(t, markerFile(f), "a rejection still has a session waiting for it")
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

	_, v := f.svc.SetSecret("FIRST_API_KEY", "sk-ant-first-00000000")
	require.True(t, v.Applied)

	f.commit(t, SetProvider("work", config.ProviderEntry{
		Driver: "anthropic", APIKey: Ref("FIRST_API_KEY"),
	}), Pending{})

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

// Clearing acknowledges one specific verdict. A marker written after it belongs to
// another waiting call, and deleting that one would strand it.
func TestClearPending_OnlyRemovesTheMarkerItResolved(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)
	f.commit(t, SetDefaultModel("claude-sonnet-5"), Pending{SessionID: 1, ToolCallID: "c1"})

	first, err := f.svc.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, first)

	f.commit(t, SetDefaultModel("anthropic/claude-sonnet-5"), Pending{SessionID: 2, ToolCallID: "c2"})

	require.NoError(t, f.svc.ClearPending(*first))

	current, err := f.svc.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, current, "the newer apply still owes its own session a verdict")
	assert.Equal(t, int64(2), current.SessionID)

	require.NoError(t, f.svc.ClearPending(*current))
	assert.NoFileExists(t, markerFile(f))
}

// A bootstrap op has no session waiting: the marker still drives the rollback,
// there is simply nobody to tell.
func TestResolvePending_BootstrapMarkerCarriesNoSession(t *testing.T) {
	f := newFixture(t, baseConfig, baseSecrets)
	f.commit(t, SetDefaultModel("anthropic/claude-sonnet-5"), Pending{})

	p, err := f.svc.LoadPending()
	require.NoError(t, err)

	out, err := f.svc.ResolvePending(*p, errEnrichment)
	require.NoError(t, err)

	assert.Equal(t, int64(0), out.Pending.SessionID)
	assert.True(t, out.RolledBack)
}
