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
