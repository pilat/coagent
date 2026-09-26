package safefile

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/pilat/coagent/internal/sandboxpolicy"
)

// ProjectPolicy admits exactly one root read-write. In-process consumers that
// are confined to a project without a session sandbox — project-context
// loading, for example — use it instead of hand-building a grant.
func ProjectPolicy(root string) sandboxpolicy.Policy {
	return sandboxpolicy.Policy{
		ProjectRoot: root,
		WorkDir:     root,
		Grants: []sandboxpolicy.Grant{{
			Source: root, Target: root, Mode: sandboxpolicy.ModeReadWrite,
			Kind: sandboxpolicy.KindDir, Present: true,
		}},
	}
}

// grant is one admitted root, the access it allows, and the identity of the
// object that was opened for it.
type grant struct {
	root     string
	target   string
	writable bool
	file     bool
	fileName string
	identity string
	handle   *os.Root
}

// openGrants holds every present grant. An absent declaration stays authorized
// but has nothing to open yet: materialization happens before this point.
func openGrants(policy sandboxpolicy.Policy) ([]grant, error) {
	grants := make([]grant, 0, len(policy.Grants))

	for _, declared := range policy.Grants {
		skip, err := inspectOptionalGrant(declared)
		if err != nil {
			closeGrants(grants)
			return nil, err
		}

		if skip {
			continue
		}

		held := grant{
			root: declared.Source, target: declared.Target,
			writable: declared.Mode == sandboxpolicy.ModeReadWrite,
			file:     declared.Kind == sandboxpolicy.KindFile,
		}

		if held.file {
			held, err = openFileGrant(held)
			if err != nil {
				closeGrants(grants)
				return nil, err
			}
		} else {
			handle, identity, err := openGrantRoot(declared.Source)
			if err != nil {
				closeGrants(grants)

				return nil, err
			}

			held.handle, held.identity = handle, identity
		}

		grants = append(grants, held)
	}

	return grants, nil
}

func inspectOptionalGrant(declared sandboxpolicy.Grant) (bool, error) {
	if declared.Present {
		return false, nil
	}

	info, err := os.Lstat(declared.Source)
	if os.IsNotExist(err) {
		if declared.Mode == sandboxpolicy.ModeReadWrite && declared.Kind == sandboxpolicy.KindDir {
			return false, fmt.Errorf("required writable grant %q was not materialized", declared.Source)
		}

		return true, nil
	}

	if err != nil {
		return false, fmt.Errorf("inspect optional grant %q: %w", declared.Source, err)
	}

	if info.Mode()&os.ModeSymlink != 0 ||
		(declared.Kind == sandboxpolicy.KindDir && !info.IsDir()) ||
		(declared.Kind == sandboxpolicy.KindFile && !info.Mode().IsRegular()) {
		return false, fmt.Errorf("optional grant %q changed kind", declared.Source)
	}

	return false, nil
}

func openFileGrant(held grant) (grant, error) {
	handle, err := openPinnedGrantRoot(filepath.Dir(held.root))
	if err != nil {
		return grant{}, fmt.Errorf("open granted file parent %q: %w", held.root, err)
	}

	file, err := openPinnedGrantFile(held.root)
	if err != nil {
		_ = handle.Close()
		return grant{}, fmt.Errorf("open granted file %q: %w", held.root, err)
	}

	info, err := file.Stat()

	_ = file.Close()
	if err != nil {
		_ = handle.Close()
		return grant{}, fmt.Errorf("stat granted file %q: %w", held.root, err)
	}

	if !info.Mode().IsRegular() {
		_ = handle.Close()
		return grant{}, fmt.Errorf("granted file %q is not regular", held.root)
	}

	held.handle = handle
	held.fileName = filepath.Base(held.root)
	held.identity = rootFileIdentity(info)

	return held, nil
}

func openGrantRoot(root string) (*os.Root, string, error) {
	handle, err := openPinnedGrantRoot(root)
	if err != nil {
		return nil, "", fmt.Errorf("open granted root %q: %w", root, err)
	}

	info, err := handle.Stat(".")
	if err != nil {
		_ = handle.Close()

		return nil, "", fmt.Errorf("identify granted root %q: %w", root, err)
	}

	identity := rootFileIdentity(info)
	if identity == "" {
		_ = handle.Close()

		return nil, "", fmt.Errorf("granted root %q has no usable identity", root)
	}

	return handle, identity, nil
}

func closeGrants(grants []grant) {
	for _, held := range grants {
		if held.handle != nil {
			_ = held.handle.Close()
		}
	}
}

// matchTarget returns the deepest grant covering a display path.
func (a *access) matchTarget(display string) (grant, string, bool) {
	var best grant

	bestRel := ""

	found := false

	for _, held := range a.grants {
		if held.file {
			if display == held.target {
				best, bestRel, found = held, ".", true
			}

			continue
		}

		relative, ok := relativeWithin(held.target, display)
		if !ok {
			continue
		}

		if !found || len(held.target) > len(best.target) {
			best, bestRel, found = held, relative, true
		}
	}

	return best, bestRel, found
}

// grantForRoot returns the grant that owns a canonical host path.
func (a *access) grantForRoot(canonical string) (grant, bool) {
	var best grant

	found := false

	for _, held := range a.grants {
		if held.file {
			if canonical == held.root {
				best, found = held, true
			}

			continue
		}

		if !pathWithinRoot(canonical, held.root) {
			continue
		}

		if !found || len(held.root) > len(best.root) {
			best, found = held, true
		}
	}

	return best, found
}

// handleFor returns the held root handle a resolved path must be opened
// through, or nil for host-readable access and single-file grants.
func (a *access) handleFor(path Path) *os.Root {
	for _, held := range a.grants {
		if held.root == path.ReadRoot && held.handle != nil && !held.file {
			return held.handle
		}
	}

	return nil
}

func (a *access) fileGrantFor(path Path) (grant, bool) {
	for _, held := range a.grants {
		if held.file && held.root == path.ReadRoot {
			return held, true
		}
	}

	return grant{}, false
}

func openExactGrantFile(held grant, flag int, perm fs.FileMode) (*os.File, error) {
	openFlag := flag &^ (os.O_TRUNC | os.O_CREATE)

	file, err := held.handle.OpenFile(held.fileName, openFlag, perm)
	if err != nil {
		return nil, fmt.Errorf("open granted file %q: %w", held.root, err)
	}

	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || rootFileIdentity(info) != held.identity {
		_ = file.Close()
		return nil, fmt.Errorf("granted file %q changed identity", held.root)
	}

	if flag&os.O_TRUNC != 0 {
		if err := file.Truncate(0); err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("truncate granted file %q: %w", held.root, err)
		}
	}

	return file, nil
}

func (a *access) authorizeWrite(path Path, flag int) error {
	if !isWriteFlag(flag) || path.Writable {
		return nil
	}

	return outsideError(path.Display)
}

func isWriteFlag(flag int) bool {
	return flag&(os.O_WRONLY|os.O_RDWR|os.O_APPEND|os.O_CREATE|os.O_TRUNC) != 0
}

func pathWithinRoot(path, root string) bool {
	_, ok := relativeWithin(root, path)

	return ok
}
