package managercontrol

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func setupClone(t *testing.T) string {
	t.Helper()

	return setupNamedClone(t, "clone")
}

func setupNamedClone(t *testing.T, name string) string {
	t.Helper()

	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	seed := filepath.Join(base, "seed")
	clone := filepath.Join(base, name)

	require.NoError(t, exec.Command("git", "init", "-q", "--bare", "-b", "trunk", remote).Run())

	require.NoError(t, exec.Command("git", "init", "-q", "-b", "trunk", seed).Run())
	runGit(t, seed, "config", "user.email", "t@t.t")
	runGit(t, seed, "config", "user.name", "t")
	runGit(t, seed, "config", "commit.gpgSign", "false")
	require.NoError(t, os.WriteFile(filepath.Join(seed, "f.txt"), []byte("v1"), 0o644))
	runGit(t, seed, "add", "-A")
	runGit(t, seed, "commit", "-m", "init")
	runGit(t, seed, "remote", "add", "origin", remote)
	runGit(t, seed, "push", "-q", "origin", "trunk")
	runGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/trunk")

	require.NoError(t, exec.Command("git", "clone", "-q", remote, clone).Run())
	runGit(t, clone, "config", "user.email", "t@t.t")
	runGit(t, clone, "config", "user.name", "t")
	runGit(t, clone, "config", "commit.gpgSign", "false")

	return clone
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}
