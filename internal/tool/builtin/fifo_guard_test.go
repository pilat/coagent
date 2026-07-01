//go:build unix

package builtin

import (
	"context"
	"encoding/json"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Opening a FIFO with no writer blocks in the kernel open(), uncancelable by
// ctx — so read/grep must reject non-regular files at the stat gate, before the
// os.Open. These run the call in a goroutine and fail on a hang rather than
// wedging the whole test binary.

func TestReadTool_FIFORejectedPromptly(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))

	params, _ := json.Marshal(readParams{FilePath: fifo})

	done := make(chan error, 1)
	go func() {
		_, err := newReadTool(dir).Execute(context.Background(), params)
		done <- err
	}()

	select {
	case err := <-done:
		require.Error(t, err, "read on a FIFO must return an error, not read it")
	case <-time.After(5 * time.Second):
		t.Fatal("read hung opening a writer-less FIFO")
	}
}

func TestGrepTool_FIFOPathRejectedPromptly(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))

	params, _ := json.Marshal(grepParams{Pattern: "x", Path: fifo})

	done := make(chan error, 1)
	go func() {
		_, err := newGrepTool(dir).Execute(context.Background(), params)
		done <- err
	}()

	select {
	case err := <-done:
		require.Error(t, err, "grep on a FIFO path must return an error")
	case <-time.After(5 * time.Second):
		t.Fatal("grep hung opening a writer-less FIFO")
	}
}

func TestEditTool_FIFORejectedPromptly(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))

	params, _ := json.Marshal(editParams{FilePath: fifo, OldString: "a", NewString: "b"})

	done := make(chan error, 1)
	go func() {
		_, err := newEditTool(dir, nil, directFileMutator{}).Execute(context.Background(), params)
		done <- err
	}()

	select {
	case err := <-done:
		require.Error(t, err, "edit on a FIFO must return an error")
	case <-time.After(5 * time.Second):
		t.Fatal("edit hung opening a writer-less FIFO")
	}
}

func TestWriteTool_FIFORejectedPromptly(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))

	params, _ := json.Marshal(writeParams{FilePath: fifo, Content: "x"})

	done := make(chan error, 1)
	go func() {
		_, err := newWriteTool(dir, nil, directFileMutator{}).Execute(context.Background(), params)
		done <- err
	}()

	select {
	case err := <-done:
		require.Error(t, err, "write onto a FIFO must return an error")
	case <-time.After(5 * time.Second):
		t.Fatal("write hung opening a writer-less FIFO")
	}
}

func TestApplyFilePatches_FIFORejectedPromptly(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))

	done := make(chan error, 1)
	go func() {
		done <- applyFilePatches(context.Background(), directFileMutator{}, fifo, nil)
	}()

	select {
	case err := <-done:
		require.Error(t, err, "apply_patch on a FIFO must return an error")
	case <-time.After(5 * time.Second):
		t.Fatal("apply_patch hung opening a writer-less FIFO")
	}
}

// A FIFO discovered via glob must be skipped, not hang the whole search.
func TestGrepTool_FIFOInDirSkipped(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o600))

	params, _ := json.Marshal(grepParams{Pattern: "x", Path: dir})

	done := make(chan struct{})
	go func() {
		_, _ = newGrepTool(dir).Execute(context.Background(), params)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("grep hung on a FIFO found via glob")
	}
}
