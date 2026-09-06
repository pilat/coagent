package safefile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

const ShieldDeniedMessage = "Coagent shields are raised; filesystem access is confined to the project."

type Scope uint8

const (
	HostReadable Scope = iota
	ProjectConfined
)

var ErrOutsideProject = errors.New("filesystem path is outside the project")

type Path struct {
	Display    string
	Canonical  string
	ReadRoot   string
	ReadRootID string
	Relative   string
}

type Opened struct {
	File *os.File
	Path Path
}

type Rooted struct {
	Root *os.Root
	Path Path
}

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
	Close() error
}

var _ Access = (*access)(nil)

type access struct {
	scope         Scope
	workDir       string
	canonicalRoot string
	rootIdentity  string
	root          *os.Root
}

//nolint:wsl_v5 // Root spelling and canonical root are established as one boundary.
func New(workDir string, scope Scope) (Access, error) {
	display, err := filepath.Abs(workDir)
	if err != nil {
		return nil, fmt.Errorf("resolve project spelling: %w", err)
	}
	display = filepath.Clean(display)

	canonical, err := filepath.EvalSymlinks(display)
	if err != nil && scope == ProjectConfined {
		return nil, fmt.Errorf("resolve canonical project: %w", err)
	}
	if err != nil {
		canonical = display
	}

	a := &access{scope: scope, workDir: display, canonicalRoot: canonical}
	if scope == ProjectConfined {
		info, err := os.Stat(canonical)
		if err != nil || !info.IsDir() {
			return nil, fmt.Errorf("project is not an existing directory: %s", display)
		}
		a.root, err = os.OpenRoot(canonical)
		if err != nil {
			return nil, fmt.Errorf("open project root: %w", err)
		}
		rootInfo, err := a.root.Stat(".")
		if err != nil {
			_ = a.root.Close()

			return nil, fmt.Errorf("identify project root: %w", err)
		}
		a.rootIdentity = rootFileIdentity(rootInfo)
		if a.rootIdentity == "" {
			_ = a.root.Close()

			return nil, errors.New("project root identity is unavailable on this platform")
		}
	}

	return a, nil
}

func (a *access) Scope() Scope          { return a.scope }
func (a *access) WorkDir() string       { return a.workDir }
func (a *access) CanonicalRoot() string { return a.canonicalRoot }

//nolint:wsl_v5 // Project paths are resolved through the held root before exposure.
func (a *access) Resolve(name string) (Path, error) {
	if a.scope == HostReadable {
		return a.resolveHost(name)
	}

	rel, display, err := a.projectRelative(name)
	if err != nil {
		return Path{}, err
	}
	resolved, err := a.resolveProjectRelative(rel)
	if err != nil {
		return Path{}, err
	}

	return Path{
		Display: display, Canonical: filepath.Join(a.canonicalRoot, resolved),
		ReadRoot: a.canonicalRoot, ReadRootID: a.rootIdentity, Relative: resolved,
	}, nil
}

//nolint:wsl_v5 // Target resolution permits only a missing final project component.
func (a *access) ResolveTarget(name string) (Path, error) {
	if a.scope == HostReadable {
		display, err := resolveHostPath(a.workDir, name)
		if err != nil {
			return Path{}, err
		}
		return Path{Display: display, Canonical: display}, nil
	}

	rel, display, err := a.projectRelative(name)
	if err != nil {
		return Path{}, err
	}
	resolved, err := a.resolveProjectTarget(rel)
	if err != nil {
		return Path{}, err
	}

	return Path{
		Display: display, Canonical: filepath.Join(a.canonicalRoot, resolved),
		ReadRoot: a.canonicalRoot, ReadRootID: a.rootIdentity, Relative: resolved,
	}, nil
}

//nolint:wsl_v5 // Resolution and rooted open are one filesystem authorization.
func (a *access) Open(name string) (*Opened, error) {
	path, err := a.Resolve(name)
	if err != nil {
		return nil, err
	}

	var file *os.File
	if a.root == nil {
		file, err = os.Open(path.Canonical)
	} else {
		file, err = a.root.Open(path.Relative)
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path.Display, err)
	}

	return &Opened{File: file, Path: path}, nil
}

//nolint:wsl_v5 // Target authorization and rooted open are one mutation boundary.
func (a *access) OpenFile(name string, flag int, perm fs.FileMode) (*Opened, error) {
	path, err := a.resolveForOpenFile(name, flag)
	if err != nil {
		return nil, err
	}

	var file *os.File
	if a.root == nil {
		file, err = os.OpenFile(path.Canonical, flag, perm)
	} else {
		file, err = a.root.OpenFile(path.Relative, flag, perm)
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path.Display, err)
	}

	return &Opened{File: file, Path: path}, nil
}

//nolint:wsl_v5 // Resolution and sub-root acquisition must observe the same project root.
func (a *access) OpenRoot(name string) (*Rooted, error) {
	path, err := a.Resolve(name)
	if err != nil {
		return nil, err
	}

	var root *os.Root
	if a.root == nil {
		root, err = os.OpenRoot(path.Canonical)
	} else {
		root, err = a.root.OpenRoot(path.Relative)
	}
	if err != nil {
		return nil, fmt.Errorf("open directory %s: %w", path.Display, err)
	}

	return &Rooted{Root: root, Path: path}, nil
}

//nolint:wsl_v5 // Rooted metadata and the admitted display path are returned together.
func (a *access) Stat(name string) (fs.FileInfo, Path, error) {
	path, err := a.Resolve(name)
	if err != nil {
		return nil, Path{}, err
	}

	var info fs.FileInfo
	if a.root == nil {
		info, err = os.Stat(path.Canonical)
	} else {
		info, err = a.root.Stat(path.Relative)
	}
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

func (a *access) Close() error {
	if a.root == nil {
		return nil
	}

	if err := a.root.Close(); err != nil {
		return fmt.Errorf("close project root: %w", err)
	}

	return nil
}
