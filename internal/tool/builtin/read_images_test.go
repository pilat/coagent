package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
)

func pngFile(t *testing.T, dir, name string, extra []byte) string {
	t.Helper()

	path := filepath.Join(dir, name)
	png := append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, extra...)
	require.NoError(t, os.WriteFile(path, png, 0o600))

	return path
}

func TestReadImage_ReturnsRefInsteadOfRejecting(t *testing.T) {
	dir := t.TempDir()
	path := pngFile(t, dir, "shot.png", nil)

	read := newReadTool(dir)
	result, err := read.Execute(context.Background(), json.RawMessage(`{"file_path":"shot.png"}`))
	require.NoError(t, err)

	require.Len(t, result.Images, 1)
	ref := result.Images[0]
	assert.True(t, filepath.IsAbs(ref.Path), "resolved absolute path")
	assert.Equal(t, path, ref.Path)
	assert.Equal(t, llmwire.MimeImagePng, ref.Mime)
	assert.Positive(t, ref.Size)

	// The success text embeds the resolved title/path deliberately: byte-identical
	// text across distinct images would fingerprint as a loop.
	assert.Contains(t, result.Output, filepath.Base(ref.Path))
	assert.Contains(t, result.Title, "shot.png")
}

func TestReadImage_OversizedFailsNamed(t *testing.T) {
	dir := t.TempDir()
	big := make([]byte, maxImageBytes+1)
	path := pngFile(t, dir, "big.png", big[8:])
	_ = path

	read := newReadTool(dir)
	_, err := read.Execute(context.Background(), json.RawMessage(`{"file_path":"big.png"}`))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds")
	assert.Contains(t, err.Error(), filepath.Join(dir, "big.png"))
}

func TestReadImage_IgnoresOffsetLimit(t *testing.T) {
	dir := t.TempDir()
	pngFile(t, dir, "shot.png", nil)

	read := newReadTool(dir)
	result, err := read.Execute(context.Background(),
		json.RawMessage(`{"file_path":"shot.png","offset":100,"limit":10}`))
	require.NoError(t, err)

	assert.NotEmpty(t, result.Images, "pagination params must not hide pixels")
}

func TestReadBlob_WithoutImageMagicStillRejected(t *testing.T) {
	dir := t.TempDir()

	datPath := filepath.Join(dir, "blob.dat")
	require.NoError(t, os.WriteFile(datPath, []byte{0x00, 0x01, 0x02, 0xFF, 0xFE}, 0o600))

	read := newReadTool(dir)
	_, err := read.Execute(context.Background(), json.RawMessage(`{"file_path":"blob.dat"}`))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "binary")
}

// pdf-named files whose bytes sniff as PDF stay rejected (out of scope).
func TestReadPdf_StaysRejected(t *testing.T) {
	dir := t.TempDir()

	pdf := filepath.Join(dir, "doc.pdf")
	body := []byte("%PDF-1.4 ... \x00\x01\x02")
	require.NoError(t, os.WriteFile(pdf, body, 0o600))

	read := newReadTool(dir)
	_, err := read.Execute(context.Background(), json.RawMessage(`{"file_path":"doc.pdf"}`))

	require.Error(t, err, "PDFs are deliberately out of scope")
}
