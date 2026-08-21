package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const initLifecycleServerEnv = "COAGENT_LSP_BLOCK_INITIALIZE"

func TestManager_CancellationDuringInitializeReapsProcess(t *testing.T) {
	workDir := t.TempDir()
	marker := filepath.Join(workDir, "initialize")
	restore := stopTimeout
	stopTimeout = 50 * time.Millisecond
	t.Cleanup(func() { stopTimeout = restore })

	m := &manager{
		servers: []serverConfig{{
			ID:         "fake",
			Extensions: []string{".go"},
			RootFinder: func(_, _ string) (string, error) { return workDir, nil },
			Spawn:      blockingInitializeServer(marker),
		}},
		clients: make(map[clientKey]*client),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)

	go func() {
		_, err := m.getClient(ctx, workDir, filepath.Join(workDir, "main.go"))
		result <- err
	}()

	require.Eventually(t, markerExists(marker), time.Second, time.Millisecond)
	cancel()

	select {
	case err := <-result:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("cancelled initialize did not reap the server process")
	}
}

func TestManager_CachedClientOutlivesStartingRequestCancellation(t *testing.T) {
	workDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var command *exec.Cmd

	m := &manager{
		servers: []serverConfig{{
			ID:         "fake",
			Extensions: []string{".go"},
			RootFinder: func(_, _ string) (string, error) { return workDir, nil },
			Spawn: func(ctx context.Context, _ string) (*exec.Cmd, error) {
				command = contextBoundFakeDiagnosticsServer(ctx)
				return command, nil
			},
		}},
		clients: make(map[clientKey]*client),
	}
	t.Cleanup(m.Close)

	file := filepath.Join(workDir, "main.go")
	client, err := m.getClient(ctx, workDir, file)
	require.NoError(t, err)
	require.NotNil(t, command)
	require.Nil(t, command.Cancel)

	cancel()
	cached, err := m.getClient(context.Background(), workDir, file)
	require.NoError(t, err)
	require.Same(t, client, cached)
	require.False(t, client.hasExited())
}

func init() {
	if os.Getenv(initLifecycleServerEnv) != "1" {
		return
	}

	runBlockingInitializeServer()
	os.Exit(0)
}

func runBlockingInitializeServer() {
	marker := os.Getenv("COAGENT_LSP_INIT_MARKER")
	reader := bufio.NewReader(os.Stdin)
	for {
		body, err := readLSPFrame(reader)
		if err != nil {
			return
		}

		var request struct {
			Method string `json:"method"`
		}
		if json.Unmarshal(body, &request) == nil && request.Method == "initialize" {
			_ = os.WriteFile(marker, []byte("ready"), 0o600)
		}
	}
}

func blockingInitializeServer(marker string) func(context.Context, string) (*exec.Cmd, error) {
	return func(ctx context.Context, _ string) (*exec.Cmd, error) {
		//nolint:gosec // Fixed test-binary arguments.
		cmd := exec.CommandContext(ctx, os.Args[0], "--")
		cmd.Env = []string{initLifecycleServerEnv + "=1", "COAGENT_LSP_INIT_MARKER=" + marker}

		return cmd, nil
	}
}

func contextBoundFakeDiagnosticsServer(ctx context.Context) *exec.Cmd {
	//nolint:gosec // Fixed test-binary arguments.
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestFakeDiagnosticsServer", "--")
	cmd.Env = []string{"COAGENT_FAKE_LSP=1"}

	return cmd
}

func markerExists(path string) func() bool {
	return func() bool {
		_, err := os.Stat(path)
		return err == nil
	}
}
