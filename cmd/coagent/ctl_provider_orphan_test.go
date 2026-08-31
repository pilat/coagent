package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/configapply"
	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/ctl"
)

const (
	orphanKey  = "sk-ant-orphaned-00000"
	retriedKey = "sk-ant-retried-000000"
)

var errApplyRefused = errors.New("refused for the test")

// failingOps refuses one step of the apply and delegates the rest, so a
// bootstrap provider can be driven past the credential write into a refusal.
type failingOps struct {
	configops.Service

	failStage  bool
	failCommit bool
}

func (f *failingOps) Stage(ops ...configops.Op) (*configops.Staged, configops.Verdict) {
	if f.failStage {
		return nil, configops.Reject("providers", errApplyRefused)
	}

	return f.Service.Stage(ops...)
}

func (f *failingOps) Commit(staged *configops.Staged, p configops.Pending) configops.Verdict {
	if f.failCommit {
		return configops.Reject("", errApplyRefused)
	}

	return f.Service.Commit(staged, p)
}

// newBootstrapPaths is an empty pre-onboarding home: no providers yet, and a
// secrets file the first credential lands in.
func newBootstrapPaths(t *testing.T) (string, string) {
	t.Helper()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	secretsPath := filepath.Join(dir, "secrets")

	require.NoError(t, os.WriteFile(configPath, []byte("models: []\n"), 0o600))
	require.NoError(t, os.WriteFile(secretsPath, nil, 0o600))

	return configPath, secretsPath
}

// The credential is written before the config that references it, and that order
// is the invariant: a config pointing at a ${VAR} nothing defines is fatal at the
// next boot, while the reverse is not. So an apply refused after the credential
// write leaves the key behind, on purpose. An unreferenced secret is inert —
// only whitelisted ${VAR} references in config.yaml are ever resolved, and
// secrets never reach the process environment — and rolling the file back would
// have to either delete a variable an existing provider still references or
// overwrite whatever another writer stored in the meantime.
func TestCommitProvider_ARefusedApplyKeepsTheCredential(t *testing.T) {
	tests := []struct {
		name string
		ops  failingOps
	}{
		{name: "the change never validates", ops: failingOps{failStage: true}},
		{name: "the write never lands", ops: failingOps{failCommit: true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configPath, secretsPath := newBootstrapPaths(t)

			failing := tt.ops
			failing.Service = configops.New(configPath, secretsPath)
			applier := configapply.New(&failing, func() {})

			v := commitProvider(applier, ctl.SetProviderParams{
				Name: "second", Driver: "anthropic", APIKey: orphanKey,
			})
			require.True(t, v.Failed(), "the apply is refused")

			secrets, err := config.LoadSecretsFrom(secretsPath)
			require.NoError(t, err)
			assert.Equal(t, orphanKey, secrets["SECOND_API_KEY"],
				"the credential stays: an unreferenced secret is inert, a missing one is fatal")

			cfg, err := os.ReadFile(configPath)
			require.NoError(t, err)
			assert.NotContains(t, string(cfg), "second", "nothing references it, so nothing can break on it")
		})
	}
}

// The retry is the recovery, and it needs no cleanup: a provider name maps to one
// variable, so the second attempt rewrites that line in place. A second
// assignment would be worse than the orphan — the reader is last-wins, and the
// stale one would outlive the rotation.
func TestSetProvider_ARetryAfterARefusedApplyOverwritesTheOrphan(t *testing.T) {
	configPath, secretsPath := newBootstrapPaths(t)

	failing := &failingOps{Service: configops.New(configPath, secretsPath), failStage: true}
	restarts := make(chan struct{}, 4)
	applier := configapply.New(failing, func() { restarts <- struct{}{} })

	refused := setProvider(applier, newFakeReplyHook(), ctl.SetProviderParams{
		Name: "second", Driver: "anthropic", APIKey: orphanKey,
	})
	require.True(t, refused.Failed())

	failing.failStage = false
	hook := newFakeReplyHook()

	applied := setProvider(applier, hook, ctl.SetProviderParams{
		Name: "second", Driver: "anthropic", APIKey: retriedKey,
	})
	require.False(t, applied.Failed(), "the refusal gave the apply slot back: %s", applied.Reason())

	hook.replied()
	awaitRestart(t, restarts)

	secrets, err := config.LoadSecretsFrom(secretsPath)
	require.NoError(t, err)
	assert.Equal(t, retriedKey, secrets["SECOND_API_KEY"], "the retry's value is the one that resolves")

	body, err := os.ReadFile(secretsPath)
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(body), "SECOND_API_KEY"),
		"the orphan line is replaced, never stacked")

	cfg, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.Contains(t, string(cfg), "${SECOND_API_KEY}", "and the config finally references it")
}
