//go:build !linux

package safefile

import "os"

func openPinnedGrantRoot(path string) (*os.Root, error) {
	return os.OpenRoot(path)
}

func openPinnedGrantFile(path string) (*os.File, error) {
	return os.Open(path)
}

func openResolvedFile(root *os.Root, path string, flag int, perm os.FileMode) (*os.File, error) {
	if root != nil {
		return root.OpenFile(path, flag, perm)
	}

	return os.OpenFile(path, flag, perm)
}

func openResolvedRoot(parent *os.Root, path string) (*os.Root, error) {
	if parent != nil {
		return parent.OpenRoot(path)
	}

	return os.OpenRoot(path)
}
