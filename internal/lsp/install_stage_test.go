package lsp

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStageInstallDirPublishesAtomically(t *testing.T) {
	parent := t.TempDir()
	finalRoot := filepath.Join(parent, "server", "1.0.0")
	expected := filepath.Join("bin", "server")
	populated := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)

	go func() {
		done <- stageInstallDir(finalRoot, expected, func(stage string) error {
			err := writeTestExecutable(filepath.Join(stage, expected), "server")
			close(populated)
			if err != nil {
				return err
			}
			<-release

			return nil
		})
	}()

	<-populated
	assert.NoDirExists(t, finalRoot)
	close(release)
	require.NoError(t, <-done)
	assert.FileExists(t, filepath.Join(finalRoot, expected))
}

func TestStageInstallDirCleansFailedAttempt(t *testing.T) {
	parent := t.TempDir()
	finalRoot := filepath.Join(parent, "server", "1.0.0")
	expectedErr := errors.New("install interrupted")

	err := stageInstallDir(finalRoot, filepath.Join("bin", "server"), func(stage string) error {
		require.NoError(t, writeTestExecutable(filepath.Join(stage, "bin", "server"), "partial"))

		return expectedErr
	})

	require.ErrorIs(t, err, expectedErr)
	assert.NoDirExists(t, finalRoot)
	matches, globErr := filepath.Glob(filepath.Join(filepath.Dir(finalRoot), ".lsp-install-*"))
	require.NoError(t, globErr)
	assert.Empty(t, matches)
}

func TestStageInstallDirRefusesInvalidExistingRoot(t *testing.T) {
	finalRoot := filepath.Join(t.TempDir(), "server", "1.0.0")
	require.NoError(t, os.MkdirAll(finalRoot, 0o755))
	called := false

	err := stageInstallDir(finalRoot, filepath.Join("bin", "server"), func(string) error {
		called = true

		return nil
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "remove it manually")
	assert.False(t, called)
	assert.DirExists(t, finalRoot)
}

func TestStageInstallDirRejectsSymlinkedFinalRoot(t *testing.T) {
	parent := t.TempDir()
	externalRoot := filepath.Join(t.TempDir(), "external")
	expected := filepath.Join("bin", "server")
	require.NoError(t, writeTestExecutable(filepath.Join(externalRoot, expected), "external"))
	finalRoot := filepath.Join(parent, "server")
	require.NoError(t, os.Symlink(externalRoot, finalRoot))
	called := false

	err := stageInstallDir(finalRoot, expected, func(string) error {
		called = true

		return nil
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a real directory")
	assert.False(t, called)
}

func TestStageInstallDirAcceptsContainedExecutableSymlink(t *testing.T) {
	finalRoot := filepath.Join(t.TempDir(), "server")
	expected := filepath.Join("node_modules", ".bin", "server")

	err := stageInstallDir(finalRoot, expected, func(stage string) error {
		realTarget := filepath.Join(stage, "node_modules", "server", "bin", "server")
		if err := writeTestExecutable(realTarget, "contained"); err != nil {
			return err
		}

		binDir := filepath.Join(stage, "node_modules", ".bin")
		if err := os.MkdirAll(binDir, 0o755); err != nil {
			return err
		}

		return os.Symlink(filepath.Join("..", "server", "bin", "server"), filepath.Join(stage, expected))
	})

	require.NoError(t, err)
	content, err := os.ReadFile(filepath.Join(finalRoot, expected))
	require.NoError(t, err)
	assert.Equal(t, "contained", string(content))
}

func TestStageInstallDirRejectsExecutableSymlinkOutsideRoot(t *testing.T) {
	finalRoot := filepath.Join(t.TempDir(), "server")
	expected := filepath.Join("bin", "server")
	external := filepath.Join(t.TempDir(), "external")
	require.NoError(t, writeTestExecutable(external, "external"))

	err := stageInstallDir(finalRoot, expected, func(stage string) error {
		if err := os.MkdirAll(filepath.Join(stage, "bin"), 0o755); err != nil {
			return err
		}

		return os.Symlink(external, filepath.Join(stage, expected))
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "resolves outside install root")
	assert.NoDirExists(t, finalRoot)
}

func TestStageInstallFileRejectsSymlinkedDestination(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "server")
	external := filepath.Join(t.TempDir(), "external")
	require.NoError(t, writeTestExecutable(external, "external"))
	require.NoError(t, os.Symlink(external, destination))
	called := false

	err := stageInstallFile(destination, "server", func(string) error {
		called = true

		return nil
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a real regular file")
	assert.False(t, called)
}

func TestStageInstallDirAcceptsConcurrentWinner(t *testing.T) {
	finalRoot := filepath.Join(t.TempDir(), "server", "1.0.0")
	expected := filepath.Join("bin", "server")
	firstPopulated := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	var secondPopulate atomic.Bool

	go func() {
		firstDone <- stageInstallDir(finalRoot, expected, func(stage string) error {
			err := writeTestExecutable(filepath.Join(stage, expected), "winner")
			close(firstPopulated)
			if err != nil {
				return err
			}
			<-releaseFirst

			return nil
		})
	}()

	<-firstPopulated
	go func() {
		secondDone <- stageInstallDir(finalRoot, expected, func(string) error {
			secondPopulate.Store(true)

			return nil
		})
	}()

	close(releaseFirst)
	require.NoError(t, <-firstDone)
	require.NoError(t, <-secondDone)
	assert.False(t, secondPopulate.Load())
	content, err := os.ReadFile(filepath.Join(finalRoot, expected))
	require.NoError(t, err)
	assert.Equal(t, "winner", string(content))
}

func TestRubyLauncherUsesPublishedGemHome(t *testing.T) {
	ruby, err := exec.LookPath("ruby")
	if err != nil {
		t.Skip("ruby not installed")
	}

	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "gems"), 0o755))
	require.NoError(t, os.MkdirAll(binDir, 0o755))
	require.NoError(t, writeTestExecutable(
		filepath.Join(binDir, "ruby-lsp-gem"),
		"#!/bin/sh\nprintf '%s|%s|%s' \"$GEM_HOME\" \"$GEM_PATH\" \"$1\"\n",
	))
	require.NoError(t, writeRubyLauncher(filepath.Join(binDir, "ruby-lsp")))

	cmd := exec.Command(ruby, filepath.Join(binDir, "ruby-lsp"), "--version")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	resolvedRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	gemHome := filepath.Join(resolvedRoot, "gems")
	want := gemHome + "|" + gemHome + "|--version"
	assert.Equal(t, want, string(output))
}

func writeTestExecutable(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	return os.WriteFile(path, []byte(content), 0o755)
}
