//go:build !linux

package safefile

import "os"

func openPinnedGrantRoot(path string) (*os.Root, error) {
	return os.OpenRoot(path)
}

func openPinnedGrantFile(path string) (*os.File, error) {
	return os.Open(path)
}
