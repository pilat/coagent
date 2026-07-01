package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/lsp"
)

type diagnosticsLSPManager struct {
	*mockLSPManager

	diagnostics []lsp.FileDiagnostics
	touchedAt   time.Time
	polledAt    time.Time
}

var _ lsp.Manager = (*diagnosticsLSPManager)(nil)

func newDiagnosticsManager(diagnostics []lsp.FileDiagnostics) *diagnosticsLSPManager {
	return &diagnosticsLSPManager{mockLSPManager: &mockLSPManager{}, diagnostics: diagnostics}
}

func (m *diagnosticsLSPManager) TouchFile(_ context.Context, _, _ string) error {
	m.touchedAt = time.Now()
	return nil
}

func (m *diagnosticsLSPManager) GetAllDiagnostics(
	_ context.Context,
	_ string,
	_, _ int,
) []lsp.FileDiagnostics {
	m.polledAt = time.Now()
	return m.diagnostics
}

// The tool must give the language server a settle window before polling, or it
// reports a file as clean that the server has not analysed yet.
func (m *diagnosticsLSPManager) settleWindow() time.Duration {
	return m.polledAt.Sub(m.touchedAt)
}

func TestWriteToolAppendsDiagnosticsOnlyWhenPresent(t *testing.T) {
	tests := []struct {
		name        string
		diagnostics []lsp.FileDiagnostics
		wantDiags   bool
	}{
		{name: "clean file", diagnostics: nil, wantDiags: false},
		{name: "file with errors", diagnostics: sampleDiagnostics(), wantDiags: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "main.go")

			mgr := newDiagnosticsManager(tt.diagnostics)

			raw, err := json.Marshal(writeParams{FilePath: path, Content: "package main\n"})
			require.NoError(t, err)

			result, err := newWriteTool(dir, mgr, directFileMutator{}).Execute(context.Background(), raw)
			require.NoError(t, err)

			assert.Equal(t, tt.wantDiags, containsDiagnosticsReport(result.Output))
			assert.GreaterOrEqual(t, mgr.settleWindow(), 140*time.Millisecond)
		})
	}
}

func TestEditToolAppendsDiagnosticsOnlyWhenPresent(t *testing.T) {
	tests := []struct {
		name        string
		diagnostics []lsp.FileDiagnostics
		wantDiags   bool
	}{
		{name: "clean file", diagnostics: nil, wantDiags: false},
		{name: "file with errors", diagnostics: sampleDiagnostics(), wantDiags: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "main.go")
			require.NoError(t, os.WriteFile(path, []byte("package main\n"), 0o644))

			mgr := newDiagnosticsManager(tt.diagnostics)

			raw, err := json.Marshal(editParams{FilePath: path, OldString: "main", NewString: "other"})
			require.NoError(t, err)

			result, err := newEditTool(dir, mgr, directFileMutator{}).Execute(context.Background(), raw)
			require.NoError(t, err)

			assert.Equal(t, tt.wantDiags, containsDiagnosticsReport(result.Output))
			assert.GreaterOrEqual(t, mgr.settleWindow(), 140*time.Millisecond)
		})
	}
}

func TestWriteToolTitleFallsBackToAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")

	raw, err := json.Marshal(writeParams{FilePath: path, Content: "body"})
	require.NoError(t, err)

	result, err := newWriteTool("", nil, directFileMutator{}).Execute(context.Background(), raw)
	require.NoError(t, err)

	assert.Equal(t, path, result.Title)
	assert.Equal(t, true, result.Metadata["isNew"])
	assert.Equal(t, "created", result.Metadata["action"])
}

func TestWriteToolReportsUpdateOnExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o644))

	raw, err := json.Marshal(writeParams{FilePath: path, Content: "new"})
	require.NoError(t, err)

	result, err := newWriteTool(dir, nil, directFileMutator{}).Execute(context.Background(), raw)
	require.NoError(t, err)

	assert.Equal(t, "out.txt", result.Title)
	assert.Equal(t, false, result.Metadata["isNew"])
	assert.Equal(t, "updated", result.Metadata["action"])
	assert.Contains(t, result.Output, "File updated successfully")
}

func TestWriteToolRejectsDirectoryTarget(t *testing.T) {
	dir := t.TempDir()

	raw, err := json.Marshal(writeParams{FilePath: dir, Content: "body"})
	require.NoError(t, err)

	_, err = newWriteTool(dir, nil, directFileMutator{}).Execute(context.Background(), raw)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "directory")
}

func TestLspToolConvertsPositionsToZeroBased(t *testing.T) {
	var gotLine, gotChar int

	mgr := &mockLSPManager{
		definitionFunc: func(_ context.Context, _, _ string, line, character int) ([]lsp.Location, error) {
			gotLine, gotChar = line, character
			return []lsp.Location{{URI: "file:///x.go"}}, nil
		},
	}

	raw, err := json.Marshal(lspParams{Operation: "goToDefinition", FilePath: "x.go", Line: 12, Character: 7})
	require.NoError(t, err)

	_, err = newLspTool("/work", mgr).Execute(context.Background(), raw)
	require.NoError(t, err)

	assert.Equal(t, 11, gotLine)
	assert.Equal(t, 6, gotChar)
}

func sampleDiagnostics() []lsp.FileDiagnostics {
	return []lsp.FileDiagnostics{{
		Path:        "main.go",
		Diagnostics: []lsp.Diagnostic{{Message: "undefined: x"}},
	}}
}

func containsDiagnosticsReport(output string) bool {
	return strings.Contains(output, "LSP errors detected in this file")
}

func TestEditToolTitleFallsBackToAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "src.txt")

	tests := []struct {
		name    string
		workDir string
		want    string
	}{
		{name: "relative to work dir", workDir: dir, want: "src.txt"},
		{name: "no work dir", workDir: "", want: path},
		{name: "work dir it cannot be relative to", workDir: "relative", want: path},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(path, []byte("alpha\n"), 0o644))

			raw, err := json.Marshal(editParams{FilePath: path, OldString: "alpha", NewString: "beta"})
			require.NoError(t, err)

			result, err := newEditTool(tt.workDir, nil, directFileMutator{}).Execute(context.Background(), raw)
			require.NoError(t, err)

			assert.Equal(t, tt.want, result.Title)
			assert.Equal(t, path, result.Metadata["filePath"])
		})
	}
}

func TestWriteToolTitleIgnoresUnrelatableWorkDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")

	raw, err := json.Marshal(writeParams{FilePath: path, Content: "body"})
	require.NoError(t, err)

	result, err := newWriteTool("relative", nil, directFileMutator{}).Execute(context.Background(), raw)
	require.NoError(t, err)

	assert.Equal(t, path, result.Title)
}

func TestEditToolRejectsBadInput(t *testing.T) {
	dir := t.TempDir()
	editTool := newEditTool(dir, nil, directFileMutator{})

	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{name: "malformed json", raw: `{`, wantErr: "invalid parameters"},
		{name: "missing file path", raw: `{"old_string":"a","new_string":"b"}`, wantErr: "file_path is required"},
		{name: "missing old string", raw: `{"file_path":"f.txt","new_string":"b"}`, wantErr: "old_string is required"},
		{
			name:    "missing file",
			raw:     `{"file_path":"gone.txt","old_string":"a","new_string":"b"}`,
			wantErr: "file not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := editTool.Execute(context.Background(), json.RawMessage(tt.raw))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}
