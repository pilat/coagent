package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/configapply"
	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/ctl"
	"github.com/pilat/coagent/internal/sessionevent"
)

const bootstrapKey = "sk-ant-bootstrap-000"

// referencingConfig already resolves ${WORK_API_KEY}, so writing that variable
// is a rotation rather than a first credential.
const referencingConfig = `providers:
    work:
        driver: anthropic
        api_key: ${WORK_API_KEY}
models:
    - id: claude-sonnet-5
      provider: work
`

// fakeSecretRequests is the daemon's masked-prompt registry reduced to its
// claim: exactly one answer per open request wins.
type fakeSecretRequests struct {
	mu   sync.Mutex
	open map[string]bool
}

func newFakeSecretRequests(requestIDs ...string) *fakeSecretRequests {
	f := &fakeSecretRequests{open: make(map[string]bool)}
	for _, id := range requestIDs {
		f.open[id] = true
	}

	return f
}

func (f *fakeSecretRequests) PendingSecretRequests(int64) []sessionevent.Notification { return nil }

func (f *fakeSecretRequests) CancelSecretRequest(_ context.Context, requestID string) error {
	return f.claim(requestID)
}

func (f *fakeSecretRequests) ResolveSecretRequest(_ context.Context, requestID, _ string) error {
	return f.claim(requestID)
}

func (f *fakeSecretRequests) claim(requestID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if !f.open[requestID] {
		return fmt.Errorf("no pending secret request %q", requestID)
	}

	delete(f.open, requestID)

	return nil
}

// fakeReplyHook is a control connection reduced to what a config op uses: the
// hook that runs once the answer is on the wire, and the close that says the
// connection is gone.
type fakeReplyHook struct {
	mu    sync.Mutex
	after func()
	done  chan struct{}
}

func newFakeReplyHook() *fakeReplyHook { return &fakeReplyHook{done: make(chan struct{})} }

func (f *fakeReplyHook) AfterReply(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.after = fn
}

func (f *fakeReplyHook) Done() <-chan struct{} { return f.done }

// replied is a response that reached the wire: the hook runs, then the client
// eventually goes away.
func (f *fakeReplyHook) replied() {
	f.mu.Lock()
	fn := f.after
	f.after = nil
	f.mu.Unlock()

	if fn != nil {
		fn()
	}

	close(f.done)
}

// dropped is a response that never reached the wire: the connection loop returns
// without running the hook and tears the connection down.
func (f *fakeReplyHook) dropped() { close(f.done) }

func newSecretApplier(t *testing.T) (configapply.Service, string, chan struct{}) {
	t.Helper()

	return newSecretApplierOn(t, "models: []\n")
}

func newSecretApplierOn(t *testing.T, configBody string) (configapply.Service, string, chan struct{}) {
	t.Helper()

	dir := t.TempDir()
	secretsPath := filepath.Join(dir, "secrets")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(configBody), 0o600))
	require.NoError(t, os.WriteFile(secretsPath, []byte("WORK_API_KEY=sk-ant-old-0000000000\n"), 0o600))

	ops := configops.New(filepath.Join(dir, "config.yaml"), secretsPath)
	restarts := make(chan struct{}, 4)

	return configapply.New(ops, func() { restarts <- struct{}{} }), secretsPath, restarts
}

// awaitRestart fails the test if the daemon was never asked to come back.
func awaitRestart(t *testing.T, restarts chan struct{}) {
	t.Helper()

	select {
	case <-restarts:
	case <-time.After(5 * time.Second):
		t.Fatal("a committed change never asked for the restart that carries it into the running image")
	}
}

// A masked prompt is re-pushed to every terminal that attaches, so two people
// can be typing an answer to the same request. Only the winner's value may
// reach the file — the refusal must leave nothing behind.
func TestSetSecret_LoserOfAReplayedPromptWritesNothing(t *testing.T) {
	applier, secretsPath, _ := newSecretApplier(t)
	requests := newFakeSecretRequests("req-1")
	ctx := context.Background()

	winner := setSecret(ctx, applier, requests, newFakeReplyHook(), ctl.SetSecretParams{
		Name: "TG_BOT_TOKEN", Value: "winner-token", RequestID: "req-1",
	})
	require.False(t, winner.Failed(), "the first answer takes the request: %s", winner.Reason())

	loser := setSecret(ctx, applier, requests, newFakeReplyHook(), ctl.SetSecretParams{
		Name: "TG_BOT_TOKEN", Value: "loser-token", RequestID: "req-1",
	})
	require.True(t, loser.Failed(), "the second answer is refused")
	assert.Contains(t, loser.Reason(), "TG_BOT_TOKEN", "the refusal names the variable it did not store")

	stored, err := os.ReadFile(secretsPath)
	require.NoError(t, err)
	assert.Contains(t, string(stored), "winner-token")
	assert.NotContains(t, string(stored), "loser-token",
		"a refused answer must not clobber the credential that won")
}

// A caller that cannot take the daemon-wide apply slot is refused before any
// side effect — otherwise the provider key is orphaned in the secrets file with
// no config referencing it.
func TestSetProvider_RefusedWithoutTheApplySlotWritesNothing(t *testing.T) {
	applier, secretsPath, _ := newSecretApplier(t)
	require.True(t, applier.ClaimApply(), "hold the slot on somebody else's behalf")

	v := setProvider(applier, newFakeReplyHook(), ctl.SetProviderParams{
		Name: "work", Driver: "anthropic", APIKey: "orphan-key",
	})
	require.True(t, v.Failed(), "the slot is taken")

	stored, err := os.ReadFile(secretsPath)
	require.NoError(t, err)
	assert.NotContains(t, string(stored), "orphan-key")
}

// A credential typed without an open prompt behind it is still a credential:
// the plain path keeps working.
func TestSetSecret_WithoutARequestIsStored(t *testing.T) {
	applier, secretsPath, _ := newSecretApplier(t)

	v := setSecret(context.Background(), applier, newFakeSecretRequests(), newFakeReplyHook(), ctl.SetSecretParams{
		Name: "SOME_KEY", Value: "plain-value",
	})
	require.False(t, v.Failed(), "%s", v.Reason())

	stored, err := os.ReadFile(secretsPath)
	require.NoError(t, err)
	assert.Contains(t, string(stored), "plain-value")
}

// The reply is what schedules the restart, and a reply that never reaches the
// wire skips it. The change is already committed by then: without the restart
// the daemon serves the superseded config for the rest of its life, and the
// apply slot it took stays taken — no later config change can ever be applied.
func TestSetProvider_ACommittedChangeRestartsEvenWhenTheReplyIsLost(t *testing.T) {
	applier, _, restarts := newSecretApplier(t)
	hook := newFakeReplyHook()

	v := setProvider(applier, hook, ctl.SetProviderParams{
		Name: "work", Driver: "anthropic", APIKey: bootstrapKey,
	})
	require.False(t, v.Failed(), "%s", v.Reason())
	require.Empty(t, restarts, "the restart waits for the answer to go out")

	hook.dropped()

	awaitRestart(t, restarts)
}

// The ordinary path is unchanged: the caller gets the verdict first, and the
// restart it schedules is what carries the committed change into the next image.
func TestSetProvider_RestartsOnceTheReplyIsOnTheWire(t *testing.T) {
	applier, secretsPath, restarts := newSecretApplier(t)
	hook := newFakeReplyHook()

	v := setProvider(applier, hook, ctl.SetProviderParams{
		Name: "work", Driver: "anthropic", APIKey: bootstrapKey,
	})
	require.False(t, v.Failed(), "%s", v.Reason())

	hook.replied()

	awaitRestart(t, restarts)

	stored, err := os.ReadFile(secretsPath)
	require.NoError(t, err)
	assert.Contains(t, string(stored), bootstrapKey)
}

// A rotation the file already references is only live once the process comes
// back for it. A lost reply must not leave the daemon holding the old value.
func TestSetSecret_ARotationRestartsEvenWhenTheReplyIsLost(t *testing.T) {
	applier, _, restarts := newSecretApplierOn(t, referencingConfig)
	hook := newFakeReplyHook()

	rotated := setSecret(context.Background(), applier, newFakeSecretRequests(), hook, ctl.SetSecretParams{
		Name: "WORK_API_KEY", Value: "sk-ant-rotated-111",
	})
	require.False(t, rotated.Failed(), "%s", rotated.Reason())
	require.Empty(t, restarts, "the restart waits for the answer to go out")

	hook.dropped()

	awaitRestart(t, restarts)
}
