//go:build linux

package safefile

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// A pinned directory prevents a replaced path component from redirecting a
// grant between policy validation and the first rooted file operation.
func openPinnedGrantRoot(path string) (*os.Root, error) {
	fd, err := unix.Openat2(unix.AT_FDCWD, path, &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_SYMLINKS,
	})
	if err != nil {
		return nil, fmt.Errorf("pin grant root %q: %w", path, err)
	}
	defer func() { _ = unix.Close(fd) }()

	root, err := os.OpenRoot(fmt.Sprintf("/proc/self/fd/%d", fd))
	if err != nil {
		return nil, fmt.Errorf("open pinned grant root %q: %w", path, err)
	}

	return root, nil
}

func openPinnedGrantFile(path string) (*os.File, error) {
	fd, err := unix.Openat2(unix.AT_FDCWD, path, &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_SYMLINKS,
	})
	if err != nil {
		return nil, fmt.Errorf("pin grant file %q: %w", path, err)
	}

	return os.NewFile(uintptr(fd), path), nil
}

// The policy checked a resolved path; following a new symlink would bypass
// nested deny rules even when the target remains inside the held root.
func openResolvedFile(root *os.Root, path string, flag int, perm os.FileMode) (*os.File, error) {
	parentFD := unix.AT_FDCWD
	resolve := uint64(unix.RESOLVE_NO_SYMLINKS)

	if root != nil {
		parent, err := root.Open(".")
		if err != nil {
			return nil, fmt.Errorf("open resolved parent: %w", err)
		}
		defer func() { _ = parent.Close() }()

		parentFD = int(parent.Fd())
		resolve |= unix.RESOLVE_BENEATH
	}

	mode := uint64(0)
	if flag&os.O_CREATE != 0 {
		mode = uint64(perm.Perm())
	}

	fd, err := unix.Openat2(parentFD, path, &unix.OpenHow{
		Flags: uint64(flag | unix.O_CLOEXEC), Mode: mode, Resolve: resolve,
	})
	if err != nil {
		return nil, fmt.Errorf("open resolved path %q: %w", path, err)
	}

	return os.NewFile(uintptr(fd), path), nil
}

func openResolvedRoot(parent *os.Root, path string) (*os.Root, error) {
	file, err := openResolvedFile(parent, path, unix.O_PATH|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	root, err := os.OpenRoot(fmt.Sprintf("/proc/self/fd/%d", file.Fd()))
	if err != nil {
		return nil, fmt.Errorf("open resolved root %q: %w", path, err)
	}

	return root, nil
}
