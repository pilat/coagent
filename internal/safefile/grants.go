package safefile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pilat/coagent/internal/sandboxpolicy"
)

// ProjectPolicy admits exactly one root read-write. In-process consumers that
// are confined to a project without a session sandbox — project-context
// loading, for example — use it instead of hand-building a policy.
func ProjectPolicy(root string) sandboxpolicy.Policy {
	return sandboxpolicy.Policy{
		ProjectRoot: root,
		WorkDir:     root,
		Entries: []sandboxpolicy.Entry{
			{Path: "/", Action: sandboxpolicy.ActionDeny, Kind: sandboxpolicy.KindDir, Present: true},
			{
				Path:    root,
				Action:  sandboxpolicy.ActionAllow,
				Mode:    sandboxpolicy.ModeReadWrite,
				Kind:    sandboxpolicy.KindDir,
				Present: true,
			},
		},
	}
}

// writableRoot is one read-write policy entry held open for symlink-safe
// writes: a directory root, or a single file's parent for an exact-file entry.
type writableRoot struct {
	path     string
	file     bool
	fileName string
	identity string
	handle   *os.Root
}

// openWritableRoots holds every present read-write entry. An absent directory
// entry is a misconfiguration: nothing outside the launcher creates it.
func openWritableRoots(policy sandboxpolicy.Policy) ([]writableRoot, error) {
	roots := make([]writableRoot, 0, len(policy.Entries))

	for _, entry := range policy.EffectiveEntries() {
		if entry.Action != sandboxpolicy.ActionAllow || entry.Mode != sandboxpolicy.ModeReadWrite {
			continue
		}

		root, skip, err := openWritableRoot(entry)
		if err != nil {
			closeWritableRoots(roots)

			return nil, err
		}

		if skip {
			continue
		}

		roots = append(roots, root)
	}

	return roots, nil
}

func openWritableRoot(entry sandboxpolicy.Entry) (writableRoot, bool, error) {
	if entry.Kind == sandboxpolicy.KindSocket {
		return writableRoot{}, true, nil
	}

	if entry.Kind == sandboxpolicy.KindFile {
		return openWritableFileRoot(entry)
	}

	if !entry.Present {
		if _, err := os.Stat(entry.Path); err != nil {
			return writableRoot{}, false, fmt.Errorf("required writable path %q was not materialized", entry.Path)
		}
	}

	handle, err := openPinnedGrantRoot(entry.Path)
	if err != nil {
		return writableRoot{}, false, fmt.Errorf("open writable root %q: %w", entry.Path, err)
	}

	info, err := handle.Stat(".")
	if err != nil {
		_ = handle.Close()

		return writableRoot{}, false, fmt.Errorf("identify writable root %q: %w", entry.Path, err)
	}

	if rootFileIdentity(info) == "" {
		_ = handle.Close()

		return writableRoot{}, false, errors.New("project root identity is unavailable on this platform")
	}

	return writableRoot{path: entry.Path, handle: handle, identity: rootFileIdentity(info)}, false, nil
}

func openWritableFileRoot(entry sandboxpolicy.Entry) (writableRoot, bool, error) {
	if !entry.Present {
		return writableRoot{}, true, nil
	}

	handle, err := openPinnedGrantRoot(filepath.Dir(entry.Path))
	if err != nil {
		return writableRoot{}, false, fmt.Errorf("open writable file parent %q: %w", entry.Path, err)
	}

	file, err := openPinnedGrantFile(entry.Path)
	if err != nil {
		_ = handle.Close()

		return writableRoot{}, false, fmt.Errorf("open writable file %q: %w", entry.Path, err)
	}

	info, err := file.Stat()
	_ = file.Close()

	if err != nil {
		_ = handle.Close()

		return writableRoot{}, false, fmt.Errorf("stat writable file %q: %w", entry.Path, err)
	}

	if !info.Mode().IsRegular() {
		_ = handle.Close()

		return writableRoot{}, false, fmt.Errorf("writable file %q is not regular", entry.Path)
	}

	return writableRoot{
		path: entry.Path, file: true, fileName: filepath.Base(entry.Path),
		handle: handle, identity: rootFileIdentity(info),
	}, false, nil
}

func closeWritableRoots(roots []writableRoot) {
	for _, root := range roots {
		if root.handle != nil {
			_ = root.handle.Close()
		}
	}
}

// matchWritableRoot returns the deepest writable root covering canonical, and
// canonical's path relative to it.
func (a *access) matchWritableRoot(canonical string) (writableRoot, string, bool) {
	var best writableRoot

	bestRel := ""
	found := false

	for _, root := range a.roots {
		if root.file {
			continue
		}

		relative, ok := relativeWithin(root.path, canonical)
		if !ok {
			continue
		}

		if !found || len(root.path) > len(best.path) {
			best, bestRel, found = root, relative, true
		}
	}

	return best, bestRel, found
}

// rootByPath returns the writable root recorded at path, empty string never
// matches.
func (a *access) rootByPath(path string) (writableRoot, bool) {
	if path == "" {
		return writableRoot{}, false
	}

	for _, root := range a.roots {
		if !root.file && root.path == path {
			return root, true
		}
	}

	return writableRoot{}, false
}

// fileRootFor returns the single-file writable root that owns canonical.
func (a *access) fileRootFor(canonical string) (writableRoot, bool) {
	for _, root := range a.roots {
		if root.file && root.path == canonical {
			return root, true
		}
	}

	return writableRoot{}, false
}

// identityFor returns the recorded identity of the writable root that owns
// canonical, for AuthorizeRead's replay check.
func (a *access) identityFor(canonical string) (string, bool) {
	if root, ok := a.fileRootFor(canonical); ok {
		return root.identity, true
	}

	if root, _, ok := a.matchWritableRoot(canonical); ok {
		return root.identity, true
	}

	return "", false
}

func openExactRootFile(held writableRoot, flag int, perm os.FileMode) (*os.File, error) {
	openFlag := flag &^ (os.O_TRUNC | os.O_CREATE)

	file, err := held.handle.OpenFile(held.fileName, openFlag, perm)
	if err != nil {
		return nil, fmt.Errorf("open writable file %q: %w", held.path, err)
	}

	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || rootFileIdentity(info) != held.identity {
		_ = file.Close()

		return nil, fmt.Errorf("writable file %q changed identity", held.path)
	}

	if flag&os.O_TRUNC != 0 {
		if err := file.Truncate(0); err != nil {
			_ = file.Close()

			return nil, fmt.Errorf("truncate writable file %q: %w", held.path, err)
		}
	}

	return file, nil
}
