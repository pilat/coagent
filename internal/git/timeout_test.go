package git

import (
	"context"
	"net"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNonInteractiveGitEnv(t *testing.T) {
	env := nonInteractiveGitEnv()

	assert.Contains(t, env, "GIT_TERMINAL_PROMPT=0")
	assert.Contains(t, env, "GCM_INTERACTIVE=never")
	assert.Contains(t, env, "GIT_ASKPASS=")
}

// A remote that accepts the TCP connection but never speaks (a tarpit) must not
// hang Clone forever: gitTimeout kills the process and Clone returns promptly.
func TestClient_Clone_TimesOutOnSlowRemote(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	t.Cleanup(func() { _ = ln.Close() })

	// Accept and hold connections open without ever responding.
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			t.Cleanup(func() { _ = conn.Close() })
		}
	}()

	restoreTimeout := gitTimeout
	restoreDelay := gitWaitDelay
	gitTimeout = 2 * time.Second
	gitWaitDelay = 2 * time.Second

	t.Cleanup(func() {
		gitTimeout = restoreTimeout
		gitWaitDelay = restoreDelay
	})

	repoURL := "https://" + ln.Addr().String() + "/repo.git"
	dest := filepath.Join(t.TempDir(), "cloned")

	done := make(chan error, 1)
	start := time.Now()

	go func() { done <- New().Clone(context.Background(), repoURL, dest) }()

	select {
	case err := <-done:
		require.Error(t, err, "clone from a mute remote must fail, not succeed")
		elapsed := time.Since(start)
		require.GreaterOrEqual(t, elapsed, gitTimeout-500*time.Millisecond, "must run until gitTimeout")
		require.Less(t, elapsed, 20*time.Second, "must fail on the deadline, not hang")
	case <-time.After(30 * time.Second):
		t.Fatal("Clone hung on a mute remote")
	}
}
