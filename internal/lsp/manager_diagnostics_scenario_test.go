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

func TestDiagnosticsScenario_ManagerAndFakeServerPublishEmpty(t *testing.T) {
	workDir := t.TempDir()
	file := filepath.Join(workDir, "main.go")
	require.NoError(t, os.WriteFile(file, []byte("package main\n"), 0o600))

	m := &manager{
		servers: []serverConfig{{
			ID:         "fake",
			Extensions: []string{".go"},
			RootFinder: func(_, _ string) (string, error) { return workDir, nil },
			Spawn:      fakeDiagnosticsServer,
		}},
		clients: make(map[clientKey]*client),
	}
	t.Cleanup(m.Close)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	diagnostics, err := m.GetDiagnostics(ctx, workDir, file)
	require.NoError(t, err)
	require.Empty(t, diagnostics)
}

func TestFakeDiagnosticsServer(t *testing.T) {
	if os.Getenv("COAGENT_FAKE_LSP") != "1" {
		return
	}
	runFakeDiagnosticsServer()
	os.Exit(0)
}

func runFakeDiagnosticsServer() {
	reader := bufio.NewReader(os.Stdin)
	for {
		body, err := readLSPFrame(reader)
		if err != nil {
			return
		}
		if fakeServerHandle(body) {
			return
		}
	}
}

func fakeDiagnosticsServer(context.Context, string) (*exec.Cmd, error) {
	//nolint:gosec // Fixed test-binary arguments.
	cmd := exec.Command(os.Args[0], "-test.run=TestFakeDiagnosticsServer", "--")
	cmd.Env = []string{"COAGENT_FAKE_LSP=1"}
	return cmd, nil
}

func fakeServerHandle(body []byte) bool {
	var message struct {
		ID     int64           `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if json.Unmarshal(body, &message) != nil {
		return true
	}
	if message.Method == "initialize" {
		writeFakeFrame(
			map[string]any{
				"jsonrpc": jsonRPCVersion,
				"id":      message.ID,
				"result":  map[string]any{"capabilities": map[string]any{}},
			},
		)
	}
	if message.Method == "textDocument/didOpen" {
		var params struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"` //nolint:tagliatelle // LSP wire key.
		}
		if json.Unmarshal(message.Params, &params) != nil {
			return true
		}
		writeFakeFrame(map[string]any{
			"jsonrpc": jsonRPCVersion,
			"method":  "textDocument/publishDiagnostics",
			"params":  map[string]any{"uri": params.TextDocument.URI, "diagnostics": []any{}},
		})
	}
	if message.Method == "shutdown" {
		writeFakeFrame(map[string]any{"jsonrpc": jsonRPCVersion, "id": message.ID, "result": nil})
	}

	return message.Method == "exit"
}

func writeFakeFrame(message any) {
	data, err := json.Marshal(message)
	if err == nil {
		_ = writeLSPFrame(os.Stdout, data)
	}
}
