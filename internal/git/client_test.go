package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClient_Clone_Success(t *testing.T) {
	sourceDir := t.TempDir()
	initGitRepo(t, sourceDir)
	createFile(t, sourceDir, "README.md", "# Test Repo")
	commitAll(t, sourceDir, "Initial commit")

	client := New()
	destDir := t.TempDir()
	clonePath := filepath.Join(destDir, "cloned-repo")

	err := client.Clone(context.Background(), sourceDir, clonePath)
	require.NoError(t, err)

	content, err := os.ReadFile(filepath.Join(clonePath, "README.md"))
	require.NoError(t, err)
	assert.Equal(t, "# Test Repo", string(content))

	assert.DirExists(t, filepath.Join(clonePath, ".git"))
}

func TestClient_Clone_AlreadyExists(t *testing.T) {
	sourceDir := t.TempDir()
	initGitRepo(t, sourceDir)

	destDir := t.TempDir()
	clonePath := filepath.Join(destDir, "existing")
	require.NoError(t, os.MkdirAll(clonePath, 0o755))
	createFile(t, clonePath, "file.txt", "content")

	client := New()
	err := client.Clone(context.Background(), sourceDir, clonePath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
}

func TestClient_Clone_InvalidSource(t *testing.T) {
	client := New()
	err := client.Clone(context.Background(), "/nonexistent/path/to/repo", t.TempDir()+"/dest")
	assert.Error(t, err)
}

func TestClient_Pull_Success(t *testing.T) {
	sourceDir := t.TempDir()
	initGitRepo(t, sourceDir)
	createFile(t, sourceDir, "README.md", "# Original")
	commitAll(t, sourceDir, "Initial commit")

	client := New()
	destDir := t.TempDir()
	clonePath := filepath.Join(destDir, "repo")
	require.NoError(t, client.Clone(context.Background(), sourceDir, clonePath))

	createFile(t, sourceDir, "NEW.md", "New content")
	commitAll(t, sourceDir, "Add new file")

	err := client.Pull(context.Background(), clonePath)
	require.NoError(t, err)

	content, err := os.ReadFile(filepath.Join(clonePath, "NEW.md"))
	require.NoError(t, err)
	assert.Equal(t, "New content", string(content))
}

func TestClient_Pull_NotARepo(t *testing.T) {
	client := New()
	notARepo := t.TempDir()
	err := client.Pull(context.Background(), notARepo)
	assert.Error(t, err)
}

func TestClient_Pull_InvalidPath(t *testing.T) {
	client := New()
	err := client.Pull(context.Background(), "/nonexistent/path/to/repo")
	assert.Error(t, err)
}

func TestClient_IsCloned_Exists(t *testing.T) {
	sourceDir := t.TempDir()
	initGitRepo(t, sourceDir)
	createFile(t, sourceDir, "file.txt", "content")
	commitAll(t, sourceDir, "Initial")

	client := New()
	destDir := t.TempDir()
	clonePath := filepath.Join(destDir, "repo")
	require.NoError(t, client.Clone(context.Background(), sourceDir, clonePath))

	assert.True(t, client.IsCloned(context.Background(), clonePath))
}

func TestClient_IsCloned_NotExists(t *testing.T) {
	client := New()
	assert.False(t, client.IsCloned(context.Background(), "/nonexistent/path"))
}

func TestClient_IsCloned_NotARepo(t *testing.T) {
	client := New()
	notARepo := t.TempDir()
	createFile(t, notARepo, "file.txt", "content")
	assert.False(t, client.IsCloned(context.Background(), notARepo))
}

func TestClient_GetRemoteURL(t *testing.T) {
	sourceDir := t.TempDir()
	initGitRepo(t, sourceDir)
	createFile(t, sourceDir, "file.txt", "content")
	commitAll(t, sourceDir, "Initial")

	client := New()
	destDir := t.TempDir()
	clonePath := filepath.Join(destDir, "repo")
	require.NoError(t, client.Clone(context.Background(), sourceDir, clonePath))

	url, err := client.GetRemoteURL(context.Background(), clonePath)
	require.NoError(t, err)
	assert.Equal(t, sourceDir, url)
}

func TestClient_GetRemoteURL_NotARepo(t *testing.T) {
	client := New()
	notARepo := t.TempDir()
	_, err := client.GetRemoteURL(context.Background(), notARepo)
	assert.Error(t, err)
}

func TestClient_HealthCheck_HealthyClone(t *testing.T) {
	sourceDir := t.TempDir()
	initGitRepo(t, sourceDir)
	createFile(t, sourceDir, "file.txt", "content")
	commitAll(t, sourceDir, "Initial")

	client := New()
	clonePath := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, client.Clone(context.Background(), sourceDir, clonePath))

	assert.NoError(t, client.HealthCheck(context.Background(), clonePath))
}

// AppleDouble ._pack-*.idx junk — the production corruption — must fail the
// fsck probe: full fsck opens every pack index, unlike --connectivity-only.
// The clone goes over file:// so it carries real pack files (local-path clones
// hardlink loose objects and may have no pack dir at all).
func TestClient_HealthCheck_AppleDoubleJunk(t *testing.T) {
	sourceDir := t.TempDir()
	initGitRepo(t, sourceDir)
	createFile(t, sourceDir, "file.txt", "content")
	commitAll(t, sourceDir, "Initial")

	client := New()
	clonePath := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, client.Clone(context.Background(), "file://"+sourceDir, clonePath))

	packDir := filepath.Join(clonePath, ".git", "objects", "pack")
	packEntries, err := os.ReadDir(packDir)
	require.NoError(t, err)

	foundPack := false
	for _, e := range packEntries {
		name := e.Name()
		if !strings.HasSuffix(name, ".pack") || strings.HasPrefix(name, "._") {
			continue
		}

		foundPack = true
		// Production had sidecars for both the pack and its index.
		require.NoError(t, os.WriteFile(filepath.Join(packDir, "._"+name), []byte("AppleDouble junk"), 0o644))
		require.NoError(t, os.WriteFile(
			filepath.Join(packDir, "._"+strings.TrimSuffix(name, ".pack")+".idx"), []byte("AppleDouble junk"), 0o644))
	}
	require.True(t, foundPack, "file:// clone must contain a pack file to corrupt")

	err = client.HealthCheck(context.Background(), clonePath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "git fsck failed")
}

func TestClient_HealthCheck_CorruptIndex(t *testing.T) {
	sourceDir := t.TempDir()
	initGitRepo(t, sourceDir)
	createFile(t, sourceDir, "file.txt", "content")
	commitAll(t, sourceDir, "Initial")

	client := New()
	clonePath := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, client.Clone(context.Background(), sourceDir, clonePath))

	require.NoError(t, os.WriteFile(
		filepath.Join(clonePath, ".git", "index"), []byte("garbage-not-an-index-file"), 0o644))

	err := client.HealthCheck(context.Background(), clonePath)
	require.Error(t, err)
}

func TestClient_HealthCheck_NotARepo(t *testing.T) {
	client := New()
	notARepo := t.TempDir()
	assert.Error(t, client.HealthCheck(context.Background(), notARepo))
}

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("git", "init")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test",
		"GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=Test",
		"GIT_COMMITTER_EMAIL=test@test.com",
	)
	require.NoError(t, cmd.Run())

	// Disable git hooks for testing
	cmd = exec.Command("git", "config", "core.hooksPath", "/dev/null")
	cmd.Dir = dir
	require.NoError(t, cmd.Run())
}

func createFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func commitAll(t *testing.T, dir, message string) {
	t.Helper()
	cmd := exec.Command("git", "add", "-A")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test",
		"GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=Test",
		"GIT_COMMITTER_EMAIL=test@test.com",
	)
	require.NoError(t, cmd.Run())

	cmd = exec.Command("git", "commit", "-m", message)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test",
		"GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=Test",
		"GIT_COMMITTER_EMAIL=test@test.com",
	)
	require.NoError(t, cmd.Run())
}
