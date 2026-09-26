package safefile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/pilat/coagent/internal/sandboxpolicy"
)

const ShieldDeniedMessage = "Filesystem access is confined to the sandbox's granted paths."

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
	// grant it. A revoked grant must not be re-read through an old reference.
	AuthorizeRead(canonical, rootIdentity string) error

	Close() error
}

var _ Access = (*access)(nil)

type access struct {
	scope         Scope
	workDir       string
	canonicalRoot string
	grants        []grant
}

// New builds a session's filesystem authority from its compiled policy. A
// policy with no grants means the sandbox is disabled: reads stay unrestricted,
// matching the operator's explicit opt-out.
func New(policy sandboxpolicy.Policy, workDir string) (Access, error) {
	display, err := filepath.Abs(workDir)
	if err != nil {
		return nil, fmt.Errorf("resolve project spelling: %w", err)
	}

	display = filepath.Clean(display)

	if len(policy.Grants) == 0 {
		return &access{scope: HostReadable, workDir: display, canonicalRoot: canonicalOrSelf(display)}, nil
	}

	grants, err := openGrants(policy)
	if err != nil {
		return nil, err
	}

	canonical := policy.ProjectRoot
	if canonical == "" {
		canonical = display
	}

	return &access{scope: ProjectConfined, workDir: display, canonicalRoot: canonical, grants: grants}, nil
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
	if a.scope == HostReadable {
		return resolveHost(a.workDir, name)
	}

	return a.resolveGranted(name, false)
}

func (a *access) ResolveTarget(name string) (Path, error) {
	if a.scope == HostReadable {
		display, err := resolveHostPath(a.workDir, name)
		if err != nil {
			return Path{}, err
		}

		return Path{Display: display, Canonical: display}, nil
	}

	return a.resolveGranted(name, true)
}

func (a *access) Open(name string) (*Opened, error) {
	path, err := a.Resolve(name)
	if err != nil {
		return nil, err
	}

	if held, ok := a.fileGrantFor(path); ok {
		file, err := openExactGrantFile(held, os.O_RDONLY, 0)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", path.Display, err)
		}

		return &Opened{File: file, Path: path}, nil
	}

	handle := a.handleFor(path)
	if handle == nil {
		file, err := os.Open(path.Canonical)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", path.Display, err)
		}

		return &Opened{File: file, Path: path}, nil
	}

	file, err := handle.Open(path.Relative)
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

	if held, ok := a.fileGrantFor(path); ok {
		file, err := openExactGrantFile(held, flag, perm)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", path.Display, err)
		}

		return &Opened{File: file, Path: path}, nil
	}

	handle := a.handleFor(path)
	if handle == nil {
		file, err := os.OpenFile(path.Canonical, flag, perm)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", path.Display, err)
		}

		return &Opened{File: file, Path: path}, nil
	}

	file, err := handle.OpenFile(path.Relative, flag, perm)
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

	handle := a.handleFor(path)
	if handle == nil {
		root, err := os.OpenRoot(path.Canonical)
		if err != nil {
			return nil, fmt.Errorf("open directory %s: %w", path.Display, err)
		}

		return &Rooted{Root: root, Path: path}, nil
	}

	root, err := handle.OpenRoot(path.Relative)
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

	if held, ok := a.fileGrantFor(path); ok {
		file, err := openExactGrantFile(held, os.O_RDONLY, 0)
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

	handle := a.handleFor(path)
	if handle == nil {
		info, err := os.Stat(path.Canonical)
		if err != nil {
			return nil, Path{}, fmt.Errorf("stat %s: %w", path.Display, err)
		}

		return info, path, nil
	}

	info, err := handle.Stat(path.Relative)
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

	held, ok := a.grantForRoot(canonical)
	if !ok {
		return outsideError(canonical)
	}

	if rootIdentity == "" || held.identity != rootIdentity {
		return outsideError(canonical)
	}

	return nil
}

func (a *access) Close() error {
	for _, held := range a.grants {
		if held.handle != nil {
			_ = held.handle.Close()
		}
	}

	return nil
}
