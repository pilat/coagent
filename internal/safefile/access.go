package safefile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/pilat/coagent/internal/sandboxpolicy"
)

const DeniedMessage = "Filesystem access is denied by a sandbox rule."

type Scope uint8

const (
	HostReadable Scope = iota
	ProjectConfined
)

var ErrOutsideProject = errors.New("filesystem path is outside the granted roots")

type Path struct {
	Display    string
	Canonical  string
	ReadRoot   string
	ReadRootID string
	Relative   string
	Writable   bool
}

type Opened struct {
	File *os.File
	Path Path
}

type Rooted struct {
	Root *os.Root
	Path Path
}

// Access is the filesystem authority one session's tools share.
type Access interface {
	Scope() Scope
	WorkDir() string
	CanonicalRoot() string
	Resolve(name string) (Path, error)
	ResolveTarget(name string) (Path, error)
	Open(name string) (*Opened, error)
	OpenFile(name string, flag int, perm fs.FileMode) (*Opened, error)
	OpenRoot(name string) (*Rooted, error)
	Stat(name string) (fs.FileInfo, Path, error)
	ReadDir(name string) ([]fs.DirEntry, Path, error)

	// AuthorizeRead confirms that a previously resolved path is still readable:
	// the recorded root identity must match and the current policy must still
	// grant it. A revoked entry must not be re-read through an old reference.
	AuthorizeRead(canonical, rootIdentity string) error

	Close() error
}

var _ Access = (*access)(nil)

type access struct {
	scope         Scope
	workDir       string
	canonicalRoot string
	policy        sandboxpolicy.Policy
	roots         []writableRoot
}

// New enforces the compiled policy for file tools running inside the daemon.
// An empty policy leaves reads and writes unrestricted.
func New(policy sandboxpolicy.Policy, workDir string) (Access, error) {
	display, err := filepath.Abs(workDir)
	if err != nil {
		return nil, fmt.Errorf("resolve project spelling: %w", err)
	}

	display = filepath.Clean(display)

	if len(policy.Entries) == 0 {
		return &access{scope: HostReadable, workDir: display, canonicalRoot: canonicalOrSelf(display)}, nil
	}

	roots, err := openWritableRoots(policy)
	if err != nil {
		return nil, err
	}

	canonical := policy.ProjectRoot
	if canonical == "" {
		canonical = display
	}

	return &access{
		scope: ProjectConfined, workDir: display, canonicalRoot: canonical, policy: policy, roots: roots,
	}, nil
}

func canonicalOrSelf(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path
	}

	return resolved
}

func (a *access) Scope() Scope          { return a.scope }
func (a *access) WorkDir() string       { return a.workDir }
func (a *access) CanonicalRoot() string { return a.canonicalRoot }

func (a *access) Resolve(name string) (Path, error) {
	display, err := resolveHostPath(a.workDir, name)
	if err != nil {
		return Path{}, err
	}

	if a.scope == HostReadable {
		canonical, err := filepath.EvalSymlinks(display)
		if err != nil {
			return Path{}, fmt.Errorf("resolve path %s: %w", display, err)
		}

		return Path{Display: display, Canonical: canonical, Writable: true}, nil
	}

	// The live filesystem resolves the common case, including a symlink that
	// points outside the project — reads are broad by default. The rooted
	// fallback below only covers a renamed ancestor, where the live path is gone.
	if canonical, err := filepath.EvalSymlinks(display); err == nil {
		if !a.policy.AllowsRead(display) || !a.policy.AllowsRead(canonical) {
			return Path{}, outsideError(display)
		}

		path := Path{Display: display, Canonical: canonical, Writable: a.policy.AllowsWrite(canonical)}
		if root, relative, ok := a.matchWritableRoot(canonical); ok {
			path.ReadRoot, path.ReadRootID, path.Relative = root.path, root.identity, relative
		} else if fileRoot, ok := a.fileRootFor(canonical); ok {
			path.ReadRoot, path.ReadRootID, path.Relative = fileRoot.path, fileRoot.identity, "."
		}

		return path, nil
	}

	if root, relative, ok := a.matchWritableRoot(display); ok {
		canonical := filepath.Join(root.path, relative)
		if !a.policy.AllowsRead(display) || !a.policy.AllowsRead(canonical) {
			return Path{}, outsideError(display)
		}

		return Path{
			Display: display, Canonical: canonical, Writable: a.policy.AllowsWrite(canonical),
			ReadRoot: root.path, ReadRootID: root.identity, Relative: relative,
		}, nil
	}

	if fileRoot, ok := a.fileRootFor(display); ok {
		if !a.policy.AllowsRead(display) {
			return Path{}, outsideError(display)
		}

		return Path{
			Display: display, Canonical: display, Writable: a.policy.AllowsWrite(display),
			ReadRoot: fileRoot.path, ReadRootID: fileRoot.identity, Relative: ".",
		}, nil
	}

	return Path{}, fmt.Errorf("resolve path %s: %w", display, os.ErrNotExist)
}

func (a *access) ResolveTarget(name string) (Path, error) {
	display, err := resolveHostPath(a.workDir, name)
	if err != nil {
		return Path{}, err
	}

	if a.scope == HostReadable {
		return Path{Display: display, Canonical: display, Writable: true}, nil
	}

	canonical := canonicalPrefix(display)
	if !a.policy.AllowsRead(display) || !a.policy.AllowsRead(canonical) {
		return Path{}, outsideError(display)
	}

	if root, relative, ok := a.matchWritableRoot(canonical); ok {
		return Path{
			Display:    display,
			Canonical:  canonical,
			Writable:   a.policy.AllowsWrite(display) && a.policy.AllowsWrite(canonical),
			ReadRoot:   root.path,
			ReadRootID: root.identity,
			Relative:   relative,
		}, nil
	}

	if fileRoot, ok := a.fileRootFor(display); ok {
		return Path{
			Display: display, Canonical: display, Writable: a.policy.AllowsWrite(display),
			ReadRoot: fileRoot.path, ReadRootID: fileRoot.identity, Relative: ".",
		}, nil
	}

	return Path{Display: display, Canonical: canonical, Writable: a.policy.AllowsWrite(canonical)}, nil
}

func (a *access) Open(name string) (*Opened, error) {
	path, err := a.Resolve(name)
	if err != nil {
		return nil, err
	}

	if held, ok := a.fileRootFor(path.Display); ok {
		file, err := openExactRootFile(held, os.O_RDONLY, 0)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", path.Display, err)
		}

		return &Opened{File: file, Path: path}, nil
	}

	if root, ok := a.rootByPath(path.ReadRoot); ok {
		file, err := openResolvedFile(root.handle, path.Relative, os.O_RDONLY, 0)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", path.Display, err)
		}

		return &Opened{File: file, Path: path}, nil
	}

	file, err := a.openHostFile(path.Canonical, os.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path.Display, err)
	}

	return &Opened{File: file, Path: path}, nil
}

func (a *access) OpenFile(name string, flag int, perm fs.FileMode) (*Opened, error) {
	path, err := a.ResolveTarget(name)
	if err != nil {
		return nil, err
	}

	if err := a.authorizeWrite(path, flag); err != nil {
		return nil, err
	}

	if held, ok := a.fileRootFor(path.Display); ok {
		file, err := openExactRootFile(held, flag, perm)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", path.Display, err)
		}

		return &Opened{File: file, Path: path}, nil
	}

	if root, ok := a.rootByPath(path.ReadRoot); ok {
		file, err := openResolvedFile(root.handle, path.Relative, flag, perm)
		if err != nil {
			// A symlink that escapes the writable root is refused by os.Root
			// itself; report it the same way as any other denied write.
			return nil, outsideError(path.Display)
		}

		return &Opened{File: file, Path: path}, nil
	}

	file, err := a.openHostFile(path.Canonical, flag, perm)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path.Display, err)
	}

	return &Opened{File: file, Path: path}, nil
}

func (a *access) OpenRoot(name string) (*Rooted, error) {
	path, err := a.Resolve(name)
	if err != nil {
		return nil, err
	}

	if held, ok := a.rootByPath(path.ReadRoot); ok {
		root, err := openResolvedRoot(held.handle, path.Relative)
		if err != nil {
			return nil, fmt.Errorf("open directory %s: %w", path.Display, err)
		}

		return &Rooted{Root: root, Path: path}, nil
	}

	var root *os.Root
	if a.scope == HostReadable {
		root, err = os.OpenRoot(path.Canonical)
	} else {
		root, err = openResolvedRoot(nil, path.Canonical)
	}

	if err != nil {
		return nil, fmt.Errorf("open directory %s: %w", path.Display, err)
	}

	return &Rooted{Root: root, Path: path}, nil
}

func (a *access) Stat(name string) (fs.FileInfo, Path, error) {
	path, err := a.Resolve(name)
	if err != nil {
		return nil, Path{}, err
	}

	if held, ok := a.fileRootFor(path.Display); ok {
		file, err := openExactRootFile(held, os.O_RDONLY, 0)
		if err != nil {
			return nil, Path{}, fmt.Errorf("stat %s: %w", path.Display, err)
		}
		defer func() { _ = file.Close() }()

		info, err := file.Stat()
		if err != nil {
			return nil, Path{}, fmt.Errorf("stat %s: %w", path.Display, err)
		}

		return info, path, nil
	}

	if root, ok := a.rootByPath(path.ReadRoot); ok {
		info, err := root.handle.Stat(path.Relative)
		if err != nil {
			return nil, Path{}, fmt.Errorf("stat %s: %w", path.Display, err)
		}

		return info, path, nil
	}

	info, err := os.Stat(path.Canonical)
	if err != nil {
		return nil, Path{}, fmt.Errorf("stat %s: %w", path.Display, err)
	}

	return info, path, nil
}

func (a *access) ReadDir(name string) ([]fs.DirEntry, Path, error) {
	opened, err := a.Open(name)
	if err != nil {
		return nil, Path{}, err
	}
	defer func() { _ = opened.File.Close() }()

	entries, err := opened.File.ReadDir(-1)
	if err != nil {
		return nil, Path{}, fmt.Errorf("read directory %s: %w", opened.Path.Display, err)
	}

	return entries, opened.Path, nil
}

// AuthorizeRead re-checks a deferred read against the current authority.
func (a *access) AuthorizeRead(canonical, rootIdentity string) error {
	if a.scope == HostReadable {
		return nil
	}

	if !a.policy.AllowsRead(canonical) || !a.policy.AllowsRead(canonicalPrefix(canonical)) {
		return outsideError(canonical)
	}

	if rootIdentity == "" {
		return nil
	}

	identity, ok := a.identityFor(canonical)
	if !ok || identity != rootIdentity {
		return outsideError(canonical)
	}

	return nil
}

func (a *access) Close() error {
	for _, root := range a.roots {
		if root.handle != nil {
			_ = root.handle.Close()
		}
	}

	return nil
}

func (a *access) authorizeWrite(path Path, flag int) error {
	if !isWriteFlag(flag) || path.Writable {
		return nil
	}

	return outsideError(path.Display)
}

func (a *access) openHostFile(path string, flag int, perm fs.FileMode) (*os.File, error) {
	if a.scope == HostReadable {
		file, err := os.OpenFile(path, flag, perm)
		if err != nil {
			return nil, fmt.Errorf("open host file %q: %w", path, err)
		}

		return file, nil
	}

	return openResolvedFile(nil, path, flag, perm)
}

func isWriteFlag(flag int) bool {
	return flag&(os.O_WRONLY|os.O_RDWR|os.O_APPEND|os.O_CREATE|os.O_TRUNC) != 0
}

func outsideError(name string) error {
	return fmt.Errorf("%w: %s: %s", ErrOutsideProject, name, DeniedMessage)
}
