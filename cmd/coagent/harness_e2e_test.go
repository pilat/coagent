//go:build integration

package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/ctl"
)

const catalogDropConfig = `providers:
  fake:
    driver: openrouter
    api_key: test-key
    base_url: %s/api/v1
models:
  - id: vendor/kept
    provider: fake
  - id: vendor/dropped
    provider: fake
sandbox:
  enabled: false
`

const catalogDropModel = `{"id":"vendor/%s","name":"%s","context_length":10000,
"top_provider":{"context_length":10000,"max_completion_tokens":1000},
"pricing":{"prompt":"0.000001","completion":"0.000002"}}`

func TestHarnessE2E_CatalogDropSurvivesRestart(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "coa-harness")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	binary := buildBinary(t)
	socket := filepath.Join(home, coagenthome.DirName, coagenthome.SocketFileName)
	kept := fmt.Sprintf(catalogDropModel, "kept", "Kept")
	both := `{"data":[` + kept + "," + fmt.Sprintf(catalogDropModel, "dropped", "Dropped") + `]}`
	one := `{"data":[` + kept + `]}`
	var body atomic.Pointer[string]
	body.Store(&both)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(*body.Load()))
	}))
	t.Cleanup(srv.Close)
	path := filepath.Join(home, coagenthome.DirName, coagenthome.ConfigFileName)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	configBody := fmt.Sprintf(catalogDropConfig, srv.URL)
	require.NoError(t, os.WriteFile(path, []byte(configBody), 0o600))
	for boot := range 4 {
		assertCatalogDropBoot(t, binary, home, socket, boot == 1 || boot == 2)
		body.Store(&one)
		if boot == 2 {
			assertCatalogTypoBoot(t, binary, home, path, configBody)
			body.Store(&both)
		}
	}
}

func TestHarnessE2E_CatalogRollbackPreservesLastResolution(t *testing.T) {
	const keptEntry = "  - id: vendor/kept\n    provider: fake\n"
	home, err := os.MkdirTemp("/tmp", "coa-harness")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	binary := buildBinary(t)
	var dropped atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := "dropped"
		if r.URL.Path == "/api/v1/models" && dropped.Load() {
			id = "kept"
		}
		_, _ = fmt.Fprintf(w, `{"data":[`+catalogDropModel+`]}`, id, id)
	}))
	t.Cleanup(srv.Close)
	dir := filepath.Join(home, coagenthome.DirName)
	path := filepath.Join(dir, coagenthome.ConfigFileName)
	socket := filepath.Join(dir, coagenthome.SocketFileName)
	configBody := strings.Replace(fmt.Sprintf(catalogDropConfig, srv.URL), keptEntry, "", 1)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(path, []byte(configBody), 0o600))
	for range 2 {
		process, _ := startCapturedDaemon(t, binary, home)
		waitForStatus(t, socket, func(st ctl.StatusResult) bool { return st.ModelCount == 1 })
		stopDaemon(t, process)
		dropped.Store(true)
	}
	cache := filepath.Join(dir, "cache", "catalog", "resolved-models.json")
	before, err := os.ReadFile(cache)
	require.NoError(t, err)
	t.Cleanup(func() {
		after, err := os.ReadFile(cache)
		require.NoError(t, err)
		assert.Equal(t, string(before), string(after))
	})
	ops := configops.New(path, filepath.Join(dir, coagenthome.SecretsFileName))
	candidate := strings.Replace(configBody, srv.URL, srv.URL+"/repointed", 1)
	candidate = strings.Replace(candidate, "enabled: false", "enabled: true", 1)
	staged, verdict := ops.StageDocument([]byte(candidate))
	require.True(t, verdict.Applied, verdict.Reason())
	require.True(t, ops.Commit(staged, configops.Pending{}).Applied)
	t.Setenv("PATH", t.TempDir())
	process, stderr := startCapturedDaemon(t, binary, home)
	st := waitForStatus(t, socket, func(st ctl.StatusResult) bool { return st.ModelCount == 1 })
	stopDaemon(t, process)
	assert.Equal(t, []string{"vendor/dropped"}, st.StaleModels)
	assert.Contains(t, stderr.String(), `"rolled_back": true`)
	assert.Contains(t, stderr.String(), "bash sandbox:")
}

// The compiled daemon answers greeting, readiness, and status over the Unix
// socket with no config file and no terminal chat involved.
func TestHarnessE2E_DaemonAnswersGreetingReadinessAndStatus(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "coa-harness")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(home) })

	binary := buildBinary(t)
	socket := filepath.Join(home, coagenthome.DirName, coagenthome.SocketFileName)
	process := startDaemon(t, binary, home)
	defer func() { _ = process.Process.Kill() }()

	st := waitForStatus(t, socket, func(status ctl.StatusResult) bool { return true })
	assert.False(t, st.ConfigPresent, "no config file was provided")

	client, err := ctl.Dial(t.Context(), socket)
	require.NoError(t, err)
	defer func() { _ = client.Close() }()

	greeting := client.Greeting()
	assert.Equal(t, ctl.AppName, greeting.App)
	assert.Equal(t, ctl.ProtocolVersion, greeting.ProtocolVersion)

	// Every removed mutation is gone from the socket, not just unregistered.
	var ignored struct{}
	err = client.Call(t.Context(), "set_provider", nil, &ignored)
	require.ErrorContains(t, err, "unknown method")

	err = client.Call(t.Context(), "chat_open", nil, &ignored)
	require.ErrorContains(t, err, "unknown method")
}

func assertCatalogDropBoot(t *testing.T, binary, home, socket string, stale bool) {
	t.Helper()

	process, stderr := startCapturedDaemon(t, binary, home)
	st := waitForStatus(t, socket, func(st ctl.StatusResult) bool { return st.PID == process.Process.Pid })
	assert.Equal(t, 2, st.ModelCount)
	if stale {
		assert.Equal(t, []string{"vendor/dropped"}, st.StaleModels)
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "status")
		cmd.Env = isolatedProcessEnv(os.Environ(), home)
		out, err := cmd.Output()
		require.NoError(t, err)
		assert.Contains(t, string(out), "model vendor/dropped (stale catalog)\n")
	} else {
		assert.Empty(t, st.StaleModels)
	}
	stopDaemon(t, process)
	assert.Equal(
		t,
		map[bool]int{true: 1, false: 0}[stale],
		strings.Count(stderr.String(), "catalog_model_missing_upstream"),
	)
}

func assertCatalogTypoBoot(t *testing.T, binary, home, path, configBody string) {
	t.Helper()

	typo := strings.Replace(configBody, "sandbox:", "  - id: vendor/typo\n    provider: fake\nsandbox:", 1)
	require.NoError(t, os.WriteFile(path, []byte(typo), 0o600))
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "daemon")
	cmd.Env = isolatedProcessEnv(os.Environ(), home)
	cmd.WaitDelay = 5 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	require.NoError(t, ctx.Err())
	require.Error(t, err)
	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit)
	assert.NotZero(t, exit.ExitCode())
	assert.Contains(
		t,
		stderr.String(),
		"model catalog: model catalog resolution failed:\n  - vendor/typo (provider \"fake\"): not present in its catalog\n",
	)
	require.NoError(t, os.WriteFile(path, []byte(configBody), 0o600))
}

func startCapturedDaemon(t *testing.T, binary, home string) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()

	stderr := &bytes.Buffer{}
	cmd := exec.Command(binary, "daemon")
	cmd.Env = isolatedProcessEnv(os.Environ(), home)
	cmd.Stdout = os.Stderr
	cmd.Stderr = stderr
	cmd.WaitDelay = 5 * time.Second
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			stopDaemon(t, cmd)
		}
	})

	return cmd, stderr
}

func stopDaemon(t *testing.T, cmd *exec.Cmd) {
	t.Helper()

	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

func buildBinary(t *testing.T) string {
	t.Helper()

	out := filepath.Join(t.TempDir(), "coagent")
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Run())

	return out
}

func startDaemon(t *testing.T, binary, home string) *exec.Cmd {
	t.Helper()

	cmd := exec.Command(binary, "daemon")
	cmd.Env = isolatedProcessEnv(os.Environ(), home)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())

	t.Cleanup(func() { _ = cmd.Process.Kill() })

	return cmd
}

// waitForStatus polls until a daemon answers a status that satisfies want.
func waitForStatus(t *testing.T, socket string, want func(ctl.StatusResult) bool) ctl.StatusResult {
	t.Helper()

	deadline := time.Now().Add(2 * time.Minute)

	for time.Now().Before(deadline) {
		if st, ok := tryStatus(socket); ok && want(st) {
			return st
		}

		time.Sleep(100 * time.Millisecond)
	}

	t.Fatalf("no daemon answered on %s within the restart budget", socket)

	return ctl.StatusResult{}
}

func tryStatus(socket string) (ctl.StatusResult, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	c, err := ctl.Dial(ctx, socket)
	if err != nil {
		return ctl.StatusResult{}, false
	}

	defer func() { _ = c.Close() }()

	var st ctl.StatusResult
	err = c.Call(ctx, ctl.OpStatus, nil, &st)

	return st, err == nil
}
