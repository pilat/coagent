package builtin

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/lsp"
)

func TestDiagnosticsScenario_WriteUsesProductionManagerAndFakeRubyLSP(t *testing.T) {
	workDir := t.TempDir()
	binDir := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", binDir)
	t.Setenv("COAGENT_BUILTIN_FAKE_LSP", "1")
	require.NoError(t, os.Symlink(os.Args[0], filepath.Join(binDir, "ruby-lsp")))

	manager := lsp.NewManager(nil)
	t.Cleanup(manager.Close)
	file := filepath.Join(workDir, "main.rb")
	params, err := json.Marshal(writeParams{FilePath: file, Content: "puts 'ok'\n"})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := newWriteTool(workDir, manager, directFileMutator{}).Execute(ctx, params)
	require.NoError(t, err)
	assert.Contains(t, result.Output, "File created successfully")
	assert.NotContains(t, result.Output, "LSP errors detected")
}

func init() {
	if os.Getenv("COAGENT_BUILTIN_FAKE_LSP") == "1" {
		runBuiltinFakeLSP()
		os.Exit(0)
	}
}

func runBuiltinFakeLSP() {
	reader := bufio.NewReader(os.Stdin)
	for {
		body, err := readFakeLSPFrame(reader)
		if err != nil || handleBuiltinFakeLSP(body) {
			return
		}
	}
}

func handleBuiltinFakeLSP(body []byte) bool {
	var message struct {
		ID     int64           `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if json.Unmarshal(body, &message) != nil {
		return true
	}
	if message.Method == "initialize" {
		writeFakeLSPFrame(map[string]any{
			"jsonrpc": "2.0",
			"id":      message.ID,
			"result":  map[string]any{"capabilities": map[string]any{}},
		})
	}
	if message.Method == "textDocument/didOpen" {
		writeFreshEmptyDiagnostics(message.Params)
	}
	if message.Method == "shutdown" {
		writeFakeLSPFrame(map[string]any{"jsonrpc": "2.0", "id": message.ID, "result": nil})
	}

	return message.Method == "exit"
}

func writeFreshEmptyDiagnostics(params json.RawMessage) {
	var request struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"` //nolint:tagliatelle // LSP wire key.
	}
	if json.Unmarshal(params, &request) != nil {
		return
	}

	writeFakeLSPFrame(map[string]any{
		"jsonrpc": "2.0",
		"method":  "textDocument/publishDiagnostics",
		"params":  map[string]any{"uri": request.TextDocument.URI, "diagnostics": []any{}},
	})
}

func readFakeLSPFrame(reader *bufio.Reader) ([]byte, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	var length int
	if _, err := fmt.Sscanf(line, "Content-Length: %d", &length); err != nil {
		return nil, err
	}
	if line, err = reader.ReadString('\n'); err != nil || line != "\r\n" {
		return nil, io.ErrUnexpectedEOF
	}
	body := make([]byte, length)
	_, err = io.ReadFull(reader, body)
	return body, err
}

func writeFakeLSPFrame(message any) {
	data, err := json.Marshal(message)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(data), data)
}
