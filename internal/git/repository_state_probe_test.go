package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The probe deadline, environment sensitivity and the porcelain parser get
// their own file so the collection tests stay under the file-size budget.

func TestRepositoryState_IgnoresStderrNoise(t *testing.T) {
	dir := newTestRepo(t)
	createFile(t, dir, "README.md", "# Test")
	commitAll(t, dir, "initial")

	// Trace output goes to stderr; it must never pollute stdout parsing.
	t.Setenv("GIT_TRACE", "1")

	state, err := New().RepositoryState(context.Background(), dir)
	require.NoError(t, err)

	assert.Equal(t, RepositoryAvailable, state.Status)
	assert.Equal(t, "main", state.Branch)
}

func TestRepositoryState_UntrackedCountIgnoresUserConfig(t *testing.T) {
	dir := newTestRepo(t)
	createFile(t, dir, "base.txt", "base")
	commitAll(t, dir, "initial")
	createFile(t, dir, "untracked.txt", "new")
	gitCmd(t, dir, "config", "status.showUntrackedFiles", "no")

	state, err := New().RepositoryState(context.Background(), dir)
	require.NoError(t, err)

	assert.Equal(t, RepositoryAvailable, state.Status)
	assert.Equal(t, 1, state.Untracked)
}

func TestRepositoryState_ProbeDeadline(t *testing.T) {
	dir := newTestRepo(t)
	createFile(t, dir, "README.md", "# Test")
	commitAll(t, dir, "initial")

	// A git that hangs must hit the probe deadline, not the 10s drain grace.
	stub := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(stub, "git"),
		[]byte("#!/bin/sh\nsleep 30\n"),
		0o755,
	))
	t.Setenv("PATH", stub)

	previous := repositoryProbeTimeout
	t.Cleanup(func() { repositoryProbeTimeout = previous })
	repositoryProbeTimeout = 300 * time.Millisecond

	start := time.Now()
	state, err := New().RepositoryState(context.Background(), dir)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Equal(t, RepositoryUnavailable, state.Status)
	assert.Less(t, elapsed, 5*time.Second, "probe must stop near its deadline")
}

func TestTruncateRunes(t *testing.T) {
	// The renderer Go-quotes what it receives, so the bound is on runes here.
	assert.Len(t, []rune(truncateRunes(strings.Repeat("b", 200), 128)), 128)
	assert.Empty(t, truncateRunes("", 128))
	assert.Equal(t, "ab", truncateRunes("ab", 128))
	assert.Equal(t, "привет,", truncateRunes("привет, мир", 7))
}

func TestParsePorcelainStatus(t *testing.T) {
	t.Run("conflict pairs AA and DD", func(t *testing.T) {
		// Non-untracked columns count as staged/unstaged too; the conflict
		// flag is additive on top of that.
		counts, err := parsePorcelainStatus("AA added-both\nDD deleted-both\n")
		require.NoError(t, err)
		assert.Equal(t, 2, counts.staged)
		assert.Equal(t, 2, counts.unstaged)
		assert.Equal(t, 2, counts.conflicted)
	})

	t.Run("rename and copy codes count as staged", func(t *testing.T) {
		counts, err := parsePorcelainStatus("R  old -> new\nC  copied\n")
		require.NoError(t, err)
		assert.Equal(t, 2, counts.staged)
	})

	t.Run("malformed line fails the probe", func(t *testing.T) {
		_, err := parsePorcelainStatus("?? broken\nMshort\n")
		assert.Error(t, err)
	})

	t.Run("empty output is clean", func(t *testing.T) {
		counts, err := parsePorcelainStatus("")
		require.NoError(t, err)
		assert.Equal(t, statusCounts{}, counts)
	})
}
