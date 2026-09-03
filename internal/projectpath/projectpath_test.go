package projectpath

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
)

func TestWorktreePath_Layout(t *testing.T) {
	root := "/wt"
	got := WorktreePath(root, "/home/u/coagent", "api")

	assert.Equal(t, root, filepath.Dir(filepath.Dir(got)),
		"worktree must sit two levels below root so it is never a /new picker child")
	assert.Equal(t, "api", filepath.Base(got))
	assert.Regexp(t, `^coagent-[0-9a-f]{8}$`, filepath.Base(filepath.Dir(got)))
}

func TestWorktreePath_SeparatesSameBasename(t *testing.T) {
	a := WorktreePath("/wt", "/home/alice/api", "fix")
	b := WorktreePath("/wt", "/home/bob/api", "fix")

	assert.NotEqual(t, a, b, "two different repos named api must not collide")
	assert.Equal(t, filepath.Base(a), filepath.Base(b), "the worktree name segment is identical")
}

func TestWorktreePath_StablePerRepo(t *testing.T) {
	assert.Equal(t,
		WorktreePath("/wt", "/home/u/repo", "x"),
		WorktreePath("/wt", "/home/u/repo/", "x"),
		"namespace must be stable across trivial path spelling")
}

func TestResolveWorktreesRoot_Default(t *testing.T) {
	restore := coagenthome.Override("/fake/home")
	defer restore()

	assert.Equal(t,
		filepath.Join("/fake/home", coagenthome.DirName, coagenthome.WorktreesDirName),
		ResolveWorktreesRoot(nil))
}

func TestResolveWorktreesRoot_ConfigOverride(t *testing.T) {
	assert.Equal(t, "/custom/wt",
		ResolveWorktreesRoot(&config.UnifiedConfig{WorktreesRoot: "/custom/wt"}))
}

func TestValidateNoOverlap_RejectsFilesystemRoot(t *testing.T) {
	require.Error(t, ValidateNoOverlap("/", "/tmp"), "the filesystem root parents everything")
	require.Error(t, ValidateNoOverlap("/tmp", "/"))
	assert.NoError(t, ValidateNoOverlap("/wt", "/home/u/wt2"))
}

func TestValidateNoOverlap_ResolvesSymlinks(t *testing.T) {
	base := t.TempDir()
	projects := filepath.Join(base, "projects")
	require.NoError(t, os.MkdirAll(projects, 0o755))

	link := filepath.Join(base, "link")
	require.NoError(t, os.Symlink(projects, link))

	require.Error(t, ValidateNoOverlap(projects, link),
		"an existing symlinked root must compare against its target")

	// The worktrees root typically does not exist on the first /gwt: a
	// missing leaf under a symlinked parent must still canonicalize.
	require.Error(t, ValidateNoOverlap(projects, filepath.Join(link, "missing")))
	assert.NoError(t, ValidateNoOverlap(projects, filepath.Join(base, "other", "missing")))
}

func TestRepoNamespace_StableAcrossSymlinkAliases(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))

	alias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(filepath.Dir(repo), alias))

	assert.Equal(t,
		RepoNamespace(repo),
		RepoNamespace(filepath.Join(alias, "repo")),
		"one repository must not fork its namespace per symlink alias",
	)
}

func TestRepoDisplayName_KeepsHashWhenTruncated(t *testing.T) {
	long := strings.Repeat("r", 140)
	a := "/home/alice/" + long
	b := "/home/bob/" + long

	da, db := RepoDisplayName(a, 123), RepoDisplayName(b, 123)
	assert.NotEqual(t, da, db, "same-basename repositories must stay distinguishable after capping")
	assert.LessOrEqual(t, utf8.RuneCountInString(da), 123)
	assert.Regexp(t, `-[0-9a-f]{8}$`, da, "the hash suffix must survive truncation")
	assert.Equal(t, RepoNamespace(a), RepoDisplayName(a, 200), "a generous limit yields the full namespace")
}
