package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writePNG(t *testing.T, dir, name string, width, height int) string {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	img.Set(0, 0, color.White)

	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))

	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o600))

	return path
}

func TestReadImage_PopulatesDimensions(t *testing.T) {
	dir := t.TempDir()
	writePNG(t, dir, "shot.png", 32, 20)

	read := newReadTool(dir)
	result, err := read.Execute(context.Background(), json.RawMessage(`{"file_path":"shot.png"}`))
	require.NoError(t, err)

	require.Len(t, result.Images, 1)
	assert.Equal(t, 32, result.Images[0].Width)
	assert.Equal(t, 20, result.Images[0].Height)
}

func TestReadImage_CorruptButSniffableLacksDimensions(t *testing.T) {
	dir := t.TempDir()

	// PNG magic from the sniffer, garbage from the decoder.
	body := append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, bytes.Repeat([]byte{0}, 64)...)
	path := filepath.Join(dir, "broken.png")
	require.NoError(t, os.WriteFile(path, body, 0o600))

	read := newReadTool(dir)
	result, err := read.Execute(context.Background(), json.RawMessage(`{"file_path":"broken.png"}`))
	require.NoError(t, err, "admission gates on the sniffer, not the decoder")

	require.Len(t, result.Images, 1)
	assert.Zero(t, result.Images[0].Width)
	assert.Zero(t, result.Images[0].Height)
}

func TestReadImage_GateEnforcesBase64DerivedLimit(t *testing.T) {
	dir := t.TempDir()

	oversized := filepath.Join(dir, "big.png")
	require.NoError(t, os.WriteFile(
		oversized, append([]byte{0x89, 'P', 'N', 'G'}, make([]byte, 4<<20)...), 0o600))

	read := newReadTool(dir)
	_, err := read.Execute(
		context.Background(), json.RawMessage(`{"file_path":"big.png"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "3932160 byte limit")
	assert.Contains(t, err.Error(), "resize or convert it first")

	under := filepath.Join(dir, "small.png")
	require.NoError(t, os.WriteFile(
		under, append([]byte{0x89, 'P', 'N', 'G'}, make([]byte, 1<<20)...), 0o600))

	result, err := read.Execute(context.Background(), json.RawMessage(`{"file_path":"small.png"}`))
	require.NoError(t, err)
	require.Len(t, result.Images, 1)
}
