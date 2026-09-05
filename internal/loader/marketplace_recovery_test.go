package loader

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/git"
)

// recoveryGitClient simulates the git.Client behaviors the resolver's recovery
// ladder branches on; Clone stamps destPath with a marker file.
type recoveryGitClient struct {
	cloned    bool
	pullErr   error
	cloneErr  error
	healthErr error

	cloneCalls int
	cloneURLs  []string
}

var _ git.Client = (*recoveryGitClient)(nil)

func (f *recoveryGitClient) Clone(_ context.Context, repoURL, destPath string) error {
	f.cloneCalls++
	f.cloneURLs = append(f.cloneURLs, repoURL)

	if f.cloneErr != nil {
		return f.cloneErr
	}

	if err := os.MkdirAll(destPath, 0o755); err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(destPath, "clone-marker"), []byte("x"), 0o644)
}

func (f *recoveryGitClient) Pull(_ context.Context, _ string) error {
	return f.pullErr
}

func (f *recoveryGitClient) IsCloned(_ context.Context, _ string) bool {
	return f.cloned
}

func (f *recoveryGitClient) GetRemoteURL(_ context.Context, _ string) (string, error) {
	return "", nil
}

func (f *recoveryGitClient) HealthCheck(_ context.Context, _ string) error {
	return f.healthErr
}

func (f *recoveryGitClient) RepositoryState(_ context.Context, _ string) (git.RepositoryState, error) {
	return git.RepositoryState{Status: git.RepositoryNotRepository}, nil
}

func recoveryTestResolver(t *testing.T, gc *recoveryGitClient) (RepositoryResolver, string) {
	t.Helper()

	home := t.TempDir()
	t.Setenv("HOME", home)

	cacheDir, err := cacheDirForMarketplace("owner", "repo")
	require.NoError(t, err)

	return NewRepoResolver(gc), cacheDir
}

// A pull failure plus a failed fsck probe (the production case: AppleDouble
// ._pack-*.idx junk) means the clone itself is broken — swap it for a fresh one.
func TestRepoResolver_UnhealthyCloneIsRecloned(t *testing.T) {
	gc := &recoveryGitClient{
		cloned: true,
		pullErr: errors.New("git pull failed: exit status 1 (output: " +
			"error: index file .git/objects/pack/._pack-aa85c7d5b947a03dd06af36b9e569e42419753c9.idx is too small)"),
		healthErr: errors.New("git fsck failed: exit status 4 (output: " +
			"error: index file .git/objects/pack/._pack-aa85c7d5b947a03dd06af36b9e569e42419753c9.idx is too small)"),
	}
	resolver, cacheDir := recoveryTestResolver(t, gc)

	require.NoError(t, os.MkdirAll(cacheDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cacheDir, "broken"), []byte("x"), 0o644))

	got, err := resolver(context.Background(), "github.com/owner/repo")
	require.NoError(t, err)
	assert.Equal(t, cacheDir, got)

	_, err = os.Stat(filepath.Join(cacheDir, "broken"))
	assert.True(t, os.IsNotExist(err), "broken clone content must be gone")
	assert.FileExists(t, filepath.Join(cacheDir, "clone-marker"))
	assert.Equal(t, []string{"https://github.com/owner/repo"}, gc.cloneURLs)
	assertNoRecoveryTempDirs(t, cacheDir)
}

// The probe is the classifier: an auth/network pull failure over a sound clone
// must not trigger a re-clone attempt, the stale clone keeps serving.
func TestRepoResolver_HealthyCloneKeepsStale(t *testing.T) {
	gc := &recoveryGitClient{
		cloned: true,
		pullErr: errors.New("git pull failed: exit status 1 (output: " +
			"fatal: could not read Username for 'https://github.com': terminal prompts disabled)"),
	}
	resolver, cacheDir := recoveryTestResolver(t, gc)

	require.NoError(t, os.MkdirAll(cacheDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cacheDir, "stale"), []byte("x"), 0o644))

	got, err := resolver(context.Background(), "github.com/owner/repo")
	require.NoError(t, err)
	assert.Equal(t, cacheDir, got)

	assert.FileExists(t, filepath.Join(cacheDir, "stale"))
	assert.Equal(t, 0, gc.cloneCalls)
}

// Recovery must never destroy the only copy: if the fresh clone fails, the
// stale clone stays in place and the resolve still succeeds.
func TestRepoResolver_FailedRecloneKeepsStaleClone(t *testing.T) {
	gc := &recoveryGitClient{
		cloned:    true,
		pullErr:   errors.New("git pull failed: exit status 128 (output: fatal: index file corrupt)"),
		cloneErr:  errors.New("git clone failed: exit status 128"),
		healthErr: errors.New("git fsck failed: exit status 128 (output: fatal: index file corrupt)"),
	}
	resolver, cacheDir := recoveryTestResolver(t, gc)

	require.NoError(t, os.MkdirAll(cacheDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cacheDir, "broken"), []byte("x"), 0o644))

	got, err := resolver(context.Background(), "github.com/owner/repo")
	require.NoError(t, err, "stale clone must still be served when recovery fails")
	assert.Equal(t, cacheDir, got)

	assert.FileExists(t, filepath.Join(cacheDir, "broken"))
	assert.NoFileExists(t, filepath.Join(cacheDir, "clone-marker"))
	assert.Equal(t, 1, gc.cloneCalls)
	assertNoRecoveryTempDirs(t, cacheDir)
}

// A dir that rev-parse rejects (half-removed, corrupted) currently dead-ends
// Clone with ErrDestinationExists forever; the resolver must replace it.
func TestRepoResolver_UnusableDirIsReplaced(t *testing.T) {
	gc := &recoveryGitClient{}
	resolver, cacheDir := recoveryTestResolver(t, gc)

	require.NoError(t, os.MkdirAll(cacheDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cacheDir, "junk"), []byte("x"), 0o644))

	got, err := resolver(context.Background(), "github.com/owner/repo")
	require.NoError(t, err)
	assert.Equal(t, cacheDir, got)

	assert.NoFileExists(t, filepath.Join(cacheDir, "junk"))
	assert.FileExists(t, filepath.Join(cacheDir, "clone-marker"))
	assert.Equal(t, []string{"https://github.com/owner/repo"}, gc.cloneURLs)
}

func assertNoRecoveryTempDirs(t *testing.T, cacheDir string) {
	t.Helper()

	entries, err := os.ReadDir(filepath.Dir(cacheDir))
	require.NoError(t, err)

	for _, e := range entries {
		assert.NotContains(t, e.Name(), ".recovery-", "recovery temp dir must not linger")
	}
}
