package projectpath

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
)

const (
	defaultRoot          = "~/" + coagenthome.DirName + "/" + coagenthome.ProjectsDirName
	defaultWorktreesRoot = "~/" + coagenthome.DirName + "/" + coagenthome.WorktreesDirName
	maxProjectNameRunes  = 64
	// repoHashLen keeps the per-repo namespace segment short while still
	// separating same-basename repositories (e.g. two "api" clones).
	repoHashLen = 8
)

func ResolveRoot(unified *config.UnifiedConfig) string {
	root := defaultRoot
	if unified != nil && strings.TrimSpace(unified.ProjectsRoot) != "" {
		root = strings.TrimSpace(unified.ProjectsRoot)
	}

	return resolveRoot(root)
}

// ResolveWorktreesRoot resolves the root under which /gwt materializes worktrees,
// defaulting to ~/.coagent/worktrees. It is deliberately distinct from the
// projects root so worktree projects never land as direct children of it and thus
// never surface in the /new picker.
func ResolveWorktreesRoot(unified *config.UnifiedConfig) string {
	root := defaultWorktreesRoot
	if unified != nil && strings.TrimSpace(unified.WorktreesRoot) != "" {
		root = strings.TrimSpace(unified.WorktreesRoot)
	}

	return resolveRoot(root)
}

// WorktreePath is the absolute directory a worktree named name gets under root
// for the repository rooted at repoRoot. The per-repo segment is
// <basename>-<hash(repoRoot)>, so two repositories sharing a basename never
// collide and the layout is always at least two levels below root.
func WorktreePath(root, repoRoot, name string) string {
	return filepath.Join(root, RepoNamespace(repoRoot), name)
}

// RepoNamespace returns the stable display and filesystem namespace for a repository.
func RepoNamespace(repoRoot string) string {
	base, hash := repoIdentity(repoRoot)

	return base + "-" + hash
}

// RepoDisplayName fits "<basename>-<hash>" into limit runes, keeping the hash
// suffix: truncated basenames of different repositories stay distinguishable.
func RepoDisplayName(repoRoot string, limit int) string {
	if limit <= 0 {
		return ""
	}

	base, hash := repoIdentity(repoRoot)

	base = truncateRunes(base, limit-repoHashLen-1)
	if base == "" {
		return hash
	}

	return base + "-" + hash
}

func repoIdentity(repoRoot string) (string, string) {
	clean := repoRoot
	if abs, err := filepath.Abs(repoRoot); err == nil {
		clean = abs
	}

	// git canonicalizes every path it reports; hashing a raw spelling would
	// fork the namespace per symlink alias of one repository.
	clean = canonicalPath(clean)
	sum := sha256.Sum256([]byte(clean))

	return filepath.Base(clean), hex.EncodeToString(sum[:])[:repoHashLen]
}

func truncateRunes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}

	if utf8.RuneCountInString(s) <= limit {
		return s
	}

	return string([]rune(s)[:limit])
}

// ValidateNoOverlap rejects equal or nested paths.
func ValidateNoOverlap(left, right string) error {
	if strings.TrimSpace(left) == "" || strings.TrimSpace(right) == "" {
		return errors.New("paths must not be empty")
	}

	leftClean := canonicalPath(left)

	rightClean := canonicalPath(right)
	if leftClean == rightClean || isNested(leftClean, rightClean) || isNested(rightClean, leftClean) {
		return fmt.Errorf("paths overlap: %q and %q", left, right)
	}

	return nil
}

// isNested reports whether child lies inside parent. filepath.Clean("/") is
// "/", which a plain prefix-scan would not report as a parent.
func isNested(parent, child string) bool {
	if parent == string(filepath.Separator) {
		return true
	}

	return strings.HasPrefix(child, parent+string(filepath.Separator))
}

// canonicalPath resolves symlinks along the deepest existing prefix of p, so
// not-yet-materialized roots still compare against their real targets.
func canonicalPath(p string) string {
	p = filepath.Clean(p)
	suffix := ""

	for {
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(resolved, suffix)
		}

		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(p, suffix)
		}

		suffix = filepath.Join(filepath.Base(p), suffix)
		p = parent
	}
}

func resolveRoot(root string) string {
	if strings.HasPrefix(root, "~/") {
		if home, err := coagenthome.UserHome(); err == nil {
			root = filepath.Join(home, root[2:])
		}
	}

	if abs, err := filepath.Abs(root); err == nil {
		return abs
	}

	return filepath.Clean(root)
}

func Same(left, right string) bool {
	if left == "" || right == "" {
		return false
	}

	leftAbs, leftErr := filepath.Abs(left)

	rightAbs, rightErr := filepath.Abs(right)
	if leftErr != nil || rightErr != nil {
		return filepath.Clean(left) == filepath.Clean(right)
	}

	return filepath.Clean(leftAbs) == filepath.Clean(rightAbs)
}

func SanitizeName(raw string) (string, error) {
	name := strings.TrimSpace(raw)

	if name == "" {
		return "", errors.New("project name is empty")
	}

	if strings.ContainsAny(name, "/\\:\x00") {
		return "", errors.New(`project name must not contain "/", "\", ":", or a NUL byte`)
	}

	if name == controllerapi.CoagentSystemProjectDir {
		return "", errors.New("project name is reserved")
	}

	if strings.HasPrefix(name, ".") {
		return "", errors.New(`project name must not start with "."`)
	}

	if utf8.RuneCountInString(name) > maxProjectNameRunes {
		return "", fmt.Errorf("project name must be at most %d characters", maxProjectNameRunes)
	}

	return name, nil
}
