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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/bashsandbox"
	"github.com/pilat/coagent/internal/safefile"
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
	if deniedRead := os.Getenv("COAGENT_FAKE_LSP_DENIED_READ"); deniedRead != "" {
		if _, err := os.ReadFile(deniedRead); err == nil {
			os.Exit(41)
		}
		if err := os.WriteFile(os.Getenv("COAGENT_FAKE_LSP_DENIED_WRITE"), []byte("escaped"), 0o600); err == nil {
			os.Exit(42)
		}
		if err := os.WriteFile(os.Getenv("COAGENT_FAKE_LSP_PROJECT_WRITE"), []byte("started"), 0o600); err != nil {
			os.Exit(43)
		}
	}
	runFakeDiagnosticsServer()
	os.Exit(0)
}

func TestManager_ShieldedFakeServerCannotReadOrWriteOutsideProject(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap is not installed")
	}
	base := t.TempDir()
	project := filepath.Join(base, "project")
	outside := filepath.Join(base, "outside")
	require.NoError(t, os.Mkdir(project, 0o755))
	require.NoError(t, os.Mkdir(outside, 0o755))
	file := filepath.Join(project, "main.go")
	require.NoError(t, os.WriteFile(file, []byte("package main\n"), 0o600))
	secret := filepath.Join(outside, "secret")
	require.NoError(t, os.WriteFile(secret, []byte("secret"), 0o600))
	outsideWrite := filepath.Join(outside, "written")
	projectWrite := filepath.Join(project, "started")
	testExecutable := filepath.Join(project, "fake-lsp")
	binary, err := os.ReadFile(os.Args[0])
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(testExecutable, binary, 0o700))

	access, err := safefile.New(project, safefile.ProjectConfined)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, access.Close()) })
	runner, err := bashsandbox.New(bashsandbox.Config{
		Enabled: true, WorkDir: project, CanonicalWorkDir: access.CanonicalRoot(),
		SessionKey: "lsp-test", ReadScope: bashsandbox.ProjectConfined,
	}, nil)
	require.NoError(t, err)
	m := &manager{
		servers: []serverConfig{{
			ID: "fake", Extensions: []string{".go"},
			RootFinder: func(_, _ string) (string, error) { return project, nil },
			Spawn: func(ctx context.Context, _ string) (*exec.Cmd, error) {
				cmd := exec.CommandContext(ctx, testExecutable, "-test.run=^TestFakeDiagnosticsServer$", "--")
				cmd.Env = []string{
					"COAGENT_FAKE_LSP=1", "COAGENT_FAKE_LSP_DENIED_READ=" + secret,
					"COAGENT_FAKE_LSP_DENIED_WRITE=" + outsideWrite,
					"COAGENT_FAKE_LSP_PROJECT_WRITE=" + projectWrite,
				}
				return cmd, nil
			},
		}},
		clients: make(map[clientKey]*client), runner: runner, access: access,
	}
	t.Cleanup(m.Close)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	diagnostics, err := m.GetDiagnostics(ctx, project, file)
	require.NoError(t, err)
	assert.Empty(t, diagnostics)
	assert.FileExists(t, projectWrite)
	assert.NoFileExists(t, outsideWrite)
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
